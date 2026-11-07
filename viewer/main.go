// alpha-wifi-liveview: minimal live-view window for a Sony A7C over Wi-Fi, with
// face/eye tracking autofocus.
//
// The camera must be in PC Remote mode ("Wi-Fi Access Point" connection
// method) on the same network as this computer, and must have been paired
// once with the GUID below.
//
// Protocol: PTP/IP (TCP 15740) + Sony SDIO extension; live view frames are
// fetched with GetObject on the special handle 0xFFFFC002. Frames are served
// locally over HTTP (see CONTRACT.md) to the embedded web page in web/,
// which detects faces and posts AF points back; this program moves the
// camera's AF point and switches its Focus Area while tracking.
package main

import (
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
)

// Pairing identity: the camera remembers this GUID once paired, so it must
// stay fixed (changing it means pairing again).
var guid = []byte{0xff, 0x18, 0xec, 0x13, 0x1e, 0x62, 0x26, 0x2a, 0x8b, 0xba, 0x14, 0x74, 0xce, 0x37, 0x97, 0x5f}

const (
	appID        = "alpha-wifi-liveview" // config/cache directory name
	friendlyName = "Alpha Live View"     // shown on the camera when pairing
	ptpPort      = 15740
	lvHandle     = 0xFFFFC002

	smartphoneModeHost = "192.168.122.1"

	rcOK            = 0x2001
	rcAccessDenied  = 0x200F
	rcInvalidHandle = 0xA009 // Sony: live view not ready yet

	// Sony SDIO properties / controls. Never write PriorityMode (0xD25A).
	propFocusArea = 0xD22C // UINT16, set with SetExtDevicePropValue (0x9205)
	propBattery   = 0xD218 // INT8 battery %, -1 unknown (read-only)
	propRecording = 0xD21D // UINT8 movie recording state, 0 = idle (read-only; not yet seen changing on the A7C)
	ctrlAFPoint   = 0xD2DC // UINT32 x<<16|y, sent with ControlDevice (0x9207)
	ctrlMovieRec  = 0xD2C8 // UINT16 Movie Rec button: 2 = press, 1 = release (0x9207)

	focusWide      = 0x0001
	focusFlexSpotM = 0x0102

	// AF point coordinate space of the camera (spans the whole frame).
	afW = 640
	afH = 480

	afMinInterval = 60 * time.Millisecond // safety net between AF commands
	pollInterval  = time.Second           // camera property poll (battery, recording)
	idleTimeout   = 8 * time.Second       // exit when the page stops polling
	restoreWait   = 3 * time.Second       // max wait for focus-area restore on exit
	frameWait     = 5 * time.Second       // /frame long-poll timeout
)

//go:embed all:web
var webFiles embed.FS

// ------------------------------------------------------------------ shared state

type hub struct {
	mu     sync.Mutex
	cond   *sync.Cond
	frame  []byte
	seq    uint64
	status string
	phase  string // setup stage shown by the page, see setPhase

	tracking bool // desired: Focus Area = Flexible Spot M
	quitting bool // desired: original Focus Area, no more AF moves
	changed  bool // camera's Focus Area may differ from the original

	battery   int   // camera battery %, -1 unknown
	recording bool  // camera is recording a movie
	recSince  int64 // unix ms when recording started (0 = not recording)

	afReq      chan [2]float64    // newest AF point (normalized), buffer 1
	recReq     chan bool          // wanted recording state, buffer 1
	restoreReq chan chan struct{} // exit: closed once the focus area is restored
}

func newHub() *hub {
	h := &hub{
		status:     "Starting…",
		battery:    -1,
		afReq:      make(chan [2]float64, 1),
		recReq:     make(chan bool, 1),
		restoreReq: make(chan chan struct{}, 1),
	}
	h.cond = sync.NewCond(&h.mu)
	return h
}

// setPhase records the connection stage the page uses to guide setup:
// searching, notfound, connecting, confirm (camera shows a pairing/OK
// prompt), refused (camera rejected this app: not paired), lost, streaming.
func (h *hub) setPhase(phase, status string) {
	h.mu.Lock()
	h.phase = phase
	h.mu.Unlock()
	h.setStatus(status)
}

func (h *hub) setStatus(s string) {
	h.mu.Lock()
	if h.status != s {
		log.Println(s)
	}
	h.status = s
	h.mu.Unlock()
}

func (h *hub) publish(jpeg []byte) {
	h.mu.Lock()
	h.frame, h.status, h.phase = jpeg, "", "streaming"
	h.seq++
	h.mu.Unlock()
	h.cond.Broadcast()
}

// wakeOnDone wakes cond waiters when ctx ends, so they can notice it.
// The returned func releases the hook; call it when done waiting.
func (h *hub) wakeOnDone(ctx context.Context) func() bool {
	return context.AfterFunc(ctx, func() {
		h.mu.Lock() // so the wake-up cannot slip in before a waiter's Wait
		h.cond.Broadcast()
		h.mu.Unlock()
	})
}

// waitFrame returns the newest frame once its seq is > after, or nil when
// timeout expires or the request is cancelled. A stale after (larger than
// the current seq, e.g. after a restart) returns the current frame at once.
func (h *hub) waitFrame(ctx context.Context, after uint64, timeout time.Duration) ([]byte, uint64) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer h.wakeOnDone(ctx)()
	h.mu.Lock()
	defer h.mu.Unlock()
	for (h.seq == after || h.frame == nil) && ctx.Err() == nil {
		h.cond.Wait()
	}
	if h.frame == nil || h.seq == after {
		return nil, 0
	}
	return h.frame, h.seq
}

// requestAF queues an AF point; a pending older request is replaced.
func (h *hub) requestAF(x, y float64) {
	for {
		select {
		case h.afReq <- [2]float64{x, y}:
			return
		default:
			select {
			case <-h.afReq:
			default:
			}
		}
	}
}

// requestRecord queues a wanted recording state; a pending one is replaced.
func (h *hub) requestRecord(on bool) {
	select {
	case <-h.recReq:
	default:
	}
	select {
	case h.recReq <- on:
	default:
	}
}

// applyProps updates the camera state shown in the page from a property
// map (full dump or diff).
func (h *hub) applyProps(props map[uint16]int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := props[propBattery]; ok {
		h.battery = int(v)
	}
	if v, ok := props[propRecording]; ok {
		rec := v != 0
		if rec && !h.recording {
			h.recSince = time.Now().UnixMilli()
		}
		if !rec {
			h.recSince = 0
		}
		if rec != h.recording {
			log.Printf("recording: %v", rec)
		}
		h.recording = rec
	}
}

func (h *hub) setTracking(on bool) {
	h.mu.Lock()
	h.tracking = on
	h.mu.Unlock()
}

// restoreFocus asks the camera goroutine to put the original focus area
// back and waits (bounded) for it. Returns false on timeout.
func (h *hub) restoreFocus(timeout time.Duration) bool {
	h.mu.Lock()
	h.quitting = true
	changed := h.changed
	h.mu.Unlock()
	if !changed {
		return true // nothing to restore
	}
	done := make(chan struct{})
	select {
	case h.restoreReq <- done:
	default:
		return false // already requested
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ------------------------------------------------------------------ focus control

// controller owns the focus state; it only runs on the camera goroutine.
type controller struct {
	orig     uint16 // user's focus area, recorded once per process
	haveOrig bool
	applied  int // focus area currently on the camera, -1 = unknown

	lastAF    time.Time
	lastAFLog time.Time
	afSkipped int // AF moves not logged since lastAFLog

	lastPoll time.Time
	logProps bool             // -logprops: log every property change (diagnostics)
	prev     map[uint16]int64 // last full property dump
}

// originalFocusArea decides which focus area to restore on exit. The A7C
// returns the full property list on every poll, but a value left behind by a
// previous session (e.g. Flexible Spot after a crash) looks like the user's
// own. So the user's last non-tracking value is remembered in a file, and
// used when the camera is found on Flexible Spot M.
func originalFocusArea(cur uint16) (uint16, bool) {
	f := ""
	if dir, err := os.UserConfigDir(); err == nil {
		f = filepath.Join(dir, appID, "focus-area")
	}
	if cur != focusFlexSpotM {
		if f != "" {
			os.MkdirAll(filepath.Dir(f), 0o755)
			os.WriteFile(f, []byte(strconv.Itoa(int(cur))), 0o644)
		}
		return cur, true
	}
	if f != "" {
		if b, err := os.ReadFile(f); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
				log.Printf("camera is on Flexible Spot M; will restore the remembered %#04x", v)
				return uint16(v), true
			}
		}
	}
	return cur, true
}

// changedProps returns the entries of now that differ from prev.
func changedProps(prev, now map[uint16]int64) map[uint16]int64 {
	out := map[uint16]int64{}
	for k, v := range now {
		if pv, ok := prev[k]; !ok || pv != v {
			out[k] = v
		}
	}
	return out
}

// connected is called after each (re)connect with the focus area read from
// the camera (ok=false if it could not be read).
func (c *controller) connected(h *hub, props map[uint16]int64) {
	h.applyProps(props)
	v, ok := props[propFocusArea]
	cur := uint16(v)
	// Always (re)send the wanted focus area after connecting: the value the
	// camera reports is not enough — on the A7C the AF point only reacts
	// (and the frame is only drawn) after Flexible Spot was set in this session.
	c.applied = -1
	c.prev = props
	if ok {
		log.Printf("focus area on camera: %#04x", cur)
		if !c.haveOrig {
			c.orig, c.haveOrig = originalFocusArea(cur)
			log.Printf("original focus area: %#04x", c.orig)
		}
	} else {
		log.Println("focus area: unknown")
	}
	c.publishChanged(h)
}

// publishChanged tells the exit path whether a restore is needed.
func (c *controller) publishChanged(h *hub) {
	h.mu.Lock()
	h.changed = c.haveOrig && c.applied != int(c.orig)
	h.mu.Unlock()
}

// step applies pending focus-area / AF requests; called before each frame.
func (c *controller) step(h *hub, dev *ptpip) error {
	h.mu.Lock()
	tracking, quitting := h.tracking, h.quitting
	h.mu.Unlock()

	want, ok := uint16(focusFlexSpotM), true
	if !tracking || quitting {
		want, ok = c.orig, c.haveOrig
	}
	if ok && int(want) != c.applied {
		rc, err := dev.setFocusArea(want)
		if err != nil {
			return err
		}
		log.Printf("focus area -> %#04x (tracking=%v, quitting=%v): rc %#04x", want, tracking, quitting, rc)
		c.applied = int(want) // don't retry in a loop on rejection
		c.publishChanged(h)
	}

	if time.Since(c.lastPoll) >= pollInterval {
		c.lastPoll = time.Now()
		props, err := dev.pollProps()
		if err != nil {
			return err
		}
		if len(props) > 0 {
			diff := changedProps(c.prev, props)
			for k, v := range c.prev { // keep values a partial dump didn't include
				if _, ok := props[k]; !ok {
					props[k] = v
				}
			}
			c.prev = props
			if c.logProps && len(diff) > 0 {
				log.Printf("props changed: %s", fmtProps(diff))
			}
			if v, ok := diff[propFocusArea]; ok {
				log.Printf("camera reports focus area %#04x", v)
			}
			h.applyProps(diff)
		}
	}

	select {
	case on := <-h.recReq:
		h.mu.Lock()
		rec := h.recording
		h.mu.Unlock()
		if on != rec {
			rc, err := dev.pressRecord()
			if err != nil {
				return err
			}
			log.Printf("record %v: rc %#04x", on, rc)
			c.lastPoll = time.Time{} // poll right away to confirm
		}
	default:
	}

	if quitting {
		select {
		case done := <-h.restoreReq:
			// The camera applies the change asynchronously: read back for up
			// to ~1.5 s so the log shows whether the restore really landed.
			for i := 0; i < 6; i++ {
				props, err := dev.pollProps()
				if err != nil || props == nil {
					break
				}
				v, ok := props[propFocusArea]
				if ok && (uint16(v) == c.orig || i == 5) {
					log.Printf("focus area read back before exit: %#04x", v)
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			close(done)
		default:
		}
		return nil
	}

	if time.Since(c.lastAF) >= afMinInterval {
		select {
		case p := <-h.afReq:
			x, y := afCoords(p[0], p[1])
			rc, err := dev.moveAF(x, y)
			if err != nil {
				return err
			}
			c.lastAF = time.Now()
			if time.Since(c.lastAFLog) >= time.Second || rc != rcOK {
				log.Printf("AF -> (%d,%d): rc %#04x (+%d not logged)", x, y, rc, c.afSkipped)
				c.lastAFLog, c.afSkipped = c.lastAF, 0
			} else {
				c.afSkipped++
			}
		default:
		}
	}
	return nil
}

// afCoords maps a normalized frame position (0,0 top-left) to the camera's
// AF point coordinates.
func afCoords(x, y float64) (uint16, uint16) {
	clamp := func(v, n float64) uint16 {
		r := math.Round(v * n)
		if !(r >= 0) { // also catches NaN
			r = 0
		}
		return uint16(min(r, n-1))
	}
	return clamp(x, afW), clamp(y, afH)
}

func fmtProps(props map[uint16]int64) string {
	codes := make([]int, 0, len(props))
	for c := range props {
		codes = append(codes, int(c))
	}
	sort.Ints(codes)
	var b strings.Builder
	for _, c := range codes {
		fmt.Fprintf(&b, "%#04x=%d ", c, props[uint16(c)])
	}
	return b.String()
}

// ------------------------------------------------------------------ PTP/IP

type ptpip struct {
	cmd, evt net.Conn
	tid      uint32
}

func pkt(typ uint32, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	b := make([]byte, 8, n)
	binary.LittleEndian.PutUint32(b[0:], uint32(n))
	binary.LittleEndian.PutUint32(b[4:], typ)
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

func u32(vs ...uint32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.LittleEndian.PutUint32(b[4*i:], v)
	}
	return b
}

func readPkt(c net.Conn) (uint32, []byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[0:])
	typ := binary.LittleEndian.Uint32(hdr[4:])
	if n < 8 || n > 64<<20 {
		return 0, nil, fmt.Errorf("bad packet length %d", n)
	}
	body := make([]byte, n-8)
	_, err := io.ReadFull(c, body)
	return typ, body, err
}

func dial(host string) (*ptpip, error) {
	addr := net.JoinHostPort(host, fmt.Sprint(ptpPort))
	cmd, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	p := &ptpip{cmd: cmd}
	name := utf16.Encode([]rune(friendlyName + "\x00"))
	nb := make([]byte, 2*len(name))
	for i, r := range name {
		binary.LittleEndian.PutUint16(nb[2*i:], r)
	}
	// Long deadline: the camera may show a confirmation prompt first.
	cmd.SetDeadline(time.Now().Add(90 * time.Second))
	if _, err := cmd.Write(pkt(1, guid, nb, u32(0x00010000))); err != nil {
		p.close()
		return nil, err
	}
	typ, body, err := readPkt(cmd)
	if err != nil {
		p.close()
		return nil, err
	}
	if typ != 2 || len(body) < 4 {
		p.close()
		return nil, fmt.Errorf("%w (packet type %d)", errRefused, typ)
	}
	connNo := binary.LittleEndian.Uint32(body)

	evt, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		p.close()
		return nil, err
	}
	p.evt = evt
	evt.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := evt.Write(pkt(3, u32(connNo))); err != nil {
		p.close()
		return nil, err
	}
	if typ, _, err := readPkt(evt); err != nil || typ != 4 {
		p.close()
		return nil, fmt.Errorf("event channel init failed: type %d, %v", typ, err)
	}
	evt.SetDeadline(time.Time{})
	go p.drainEvents()
	return p, nil
}

// errRefused: the camera answered the connection request with a refusal,
// i.e. this app is not paired (or pairing was cancelled on the camera).
var errRefused = errors.New("camera refused the connection")

// logEvents (-logprops) logs PTP events, to find which ones signal e.g. the
// start/stop of a movie recording.
var logEvents bool

// Events are otherwise unused, but must be consumed; probe requests must be answered.
func (p *ptpip) drainEvents() {
	for {
		typ, body, err := readPkt(p.evt)
		if err != nil {
			return
		}
		switch typ {
		case 13: // Probe_Request
			p.evt.Write(pkt(14))
		case 8: // Event: code u16, tid u32, params u32...
			if logEvents && len(body) >= 6 {
				var params []string
				for o := 6; o+4 <= len(body); o += 4 {
					params = append(params, fmt.Sprintf("%#x", binary.LittleEndian.Uint32(body[o:])))
				}
				log.Printf("event %#04x %v", binary.LittleEndian.Uint16(body), params)
			}
		}
	}
}

func (p *ptpip) close() {
	if p.cmd != nil {
		p.cmd.Close()
	}
	if p.evt != nil {
		p.evt.Close()
	}
}

// transact runs one operation without outgoing data; returns response code and incoming data.
func (p *ptpip) transact(code uint16, params ...uint32) (uint16, []byte, error) {
	return p.exchange(code, params, nil)
}

// transactOut runs one operation with an outgoing data phase.
func (p *ptpip) transactOut(code uint16, data []byte, params ...uint32) (uint16, error) {
	rc, _, err := p.exchange(code, params, data)
	return rc, err
}

// exchange sends an operation request, then out (if non-nil) as a data-out
// phase (StartData + EndData), and reads the response and any data-in.
func (p *ptpip) exchange(code uint16, params []uint32, out []byte) (uint16, []byte, error) {
	p.tid++
	req := make([]byte, 10)
	phase := uint32(1) // data phase: none/in
	if out != nil {
		phase = 2 // data out
	}
	binary.LittleEndian.PutUint32(req[0:], phase)
	binary.LittleEndian.PutUint16(req[4:], code)
	binary.LittleEndian.PutUint32(req[6:], p.tid)
	p.cmd.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := p.cmd.Write(pkt(6, req, u32(params...))); err != nil {
		return 0, nil, err
	}
	if out != nil {
		start := binary.LittleEndian.AppendUint64(u32(p.tid), uint64(len(out)))
		if _, err := p.cmd.Write(pkt(9, start)); err != nil {
			return 0, nil, err
		}
		if _, err := p.cmd.Write(pkt(12, u32(p.tid), out)); err != nil {
			return 0, nil, err
		}
	}
	var data []byte
	for {
		typ, body, err := readPkt(p.cmd)
		if err != nil {
			return 0, nil, err
		}
		switch typ {
		case 9: // start data: tid + total length
			if len(body) >= 12 {
				data = make([]byte, 0, binary.LittleEndian.Uint64(body[4:]))
			}
		case 10, 12: // data, end data
			if len(body) >= 4 {
				data = append(data, body[4:]...)
			}
		case 7: // operation response
			if len(body) < 2 {
				return 0, nil, errors.New("short response")
			}
			return binary.LittleEndian.Uint16(body), data, nil
		}
	}
}

// Camera commands used by the controller. rc is the PTP response code;
// err means the link is broken.

func (p *ptpip) setFocusArea(v uint16) (uint16, error) {
	return p.transactOut(0x9205, binary.LittleEndian.AppendUint16(nil, v), propFocusArea)
}

func (p *ptpip) moveAF(x, y uint16) (uint16, error) {
	return p.transactOut(0x9207, u32(uint32(x)<<16|uint32(y)), ctrlAFPoint)
}

func (p *ptpip) pressRecord() (uint16, error) {
	rc, err := p.transactOut(0x9207, binary.LittleEndian.AppendUint16(nil, 2), ctrlMovieRec)
	if err != nil || rc != rcOK {
		return rc, err
	}
	time.Sleep(100 * time.Millisecond)
	return p.transactOut(0x9207, binary.LittleEndian.AppendUint16(nil, 1), ctrlMovieRec)
}

// pollProps returns the properties that changed since the previous 0x9209.
func (p *ptpip) pollProps() (map[uint16]int64, error) {
	rc, d, err := p.transact(0x9209) // SDIO_GetAllExtDevicePropInfo
	if err != nil {
		return nil, err
	}
	if rc != rcOK {
		return nil, nil
	}
	props, perr := parseExtProps(d)
	if perr != nil {
		log.Println("property poll:", perr)
		return nil, nil
	}
	return props, nil
}

func (p *ptpip) sonyHandshake() error {
	steps := []struct {
		code   uint16
		params []uint32
	}{
		{0x1001, nil},               // GetDeviceInfo
		{0x1002, []uint32{1}},       // OpenSession
		{0x9201, []uint32{1, 0, 0}}, // SDIO_Connect phase 1
		{0x9201, []uint32{2, 0, 0}}, // SDIO_Connect phase 2
	}
	for _, s := range steps {
		rc, _, err := p.transact(s.code, s.params...)
		if err != nil {
			return err
		}
		if rc != rcOK {
			return fmt.Errorf("operation %#04x failed: %#04x", s.code, rc)
		}
	}
	ok := false
	for i := 0; i < 20 && !ok; i++ {
		rc, d, err := p.transact(0x9202, 0x12C) // SDIO_GetExtDeviceInfo, protocol 3.00
		if err != nil {
			return err
		}
		ok = rc == rcOK && len(d) > 0
		if !ok {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !ok {
		return errors.New("camera did not return extended device info")
	}
	rc, _, err := p.transact(0x9201, 3, 0, 0) // SDIO_Connect phase 3
	if err == nil && rc != rcOK {
		err = fmt.Errorf("SDIO_Connect(3) failed: %#04x", rc)
	}
	return err
}

// readProps reads the full property dump. Must be the first 0x9209 of the
// connection: later ones only return properties that changed. err is only
// set if the link is broken; an unreadable dump gives an empty map.
func (p *ptpip) readProps() (map[uint16]int64, error) {
	props, err := p.pollProps()
	if props == nil {
		props = map[uint16]int64{}
	}
	return props, err
}

// stream pulls live view frames until an error occurs.
func (p *ptpip) stream(h *hub, c *controller) error {
	frames, t0 := 0, time.Now()
	for {
		if err := c.step(h, p); err != nil {
			return err
		}
		rc, d, err := p.transact(0x1009, lvHandle) // GetObject
		if err != nil {
			return err
		}
		if rc == rcAccessDenied || rc == rcInvalidHandle {
			time.Sleep(30 * time.Millisecond)
			continue
		}
		if rc != rcOK {
			return fmt.Errorf("live view failed: %#04x", rc)
		}
		if len(d) < 8 {
			continue
		}
		off := binary.LittleEndian.Uint32(d[0:])
		size := binary.LittleEndian.Uint32(d[4:])
		if uint64(off)+uint64(size) > uint64(len(d)) || size < 4 || d[off] != 0xFF || d[off+1] != 0xD8 {
			continue
		}
		jpeg := make([]byte, size)
		copy(jpeg, d[off:off+size])
		h.publish(jpeg)
		frames++
		if el := time.Since(t0); el > 10*time.Second {
			log.Printf("%.1f fps", float64(frames)/el.Seconds())
			frames, t0 = 0, time.Now()
		}
	}
}

// ------------------------------------------------------------------ property dump

// parseExtProps parses a SDIO_GetAllExtDevicePropInfo (0x9209) dump, protocol
// 0x12C layout, and returns the current value
// of every integer property.
//
// Layout: u32 count, 4 unknown bytes, then per property: code u16, type u16,
// getset u8, enabled u8, default value, current value, form flag u8, then a
// range (min, max, step) or an enum given as two lists (u16 count + values).
func parseExtProps(d []byte) (map[uint16]int64, error) {
	if len(d) < 8 {
		return nil, errors.New("short property dump")
	}
	r := &propReader{d: d, o: 8}
	n := binary.LittleEndian.Uint32(d)
	out := make(map[uint16]int64)
	for i := uint32(0); i < n; i++ {
		code, typ := r.u16(), r.u16()
		r.take(2) // getset, enabled
		r.value(typ)
		cur, isInt := r.value(typ)
		switch r.u8() {
		case 1: // range
			r.value(typ)
			r.value(typ)
			r.value(typ)
		case 2: // enum: two lists
			for k := 0; k < 2 && r.err == nil; k++ {
				for j := int(r.u16()); j > 0 && r.err == nil; j-- {
					r.value(typ)
				}
			}
		}
		if r.err != nil {
			return nil, fmt.Errorf("property %d (%#04x): %w", i, code, r.err)
		}
		if isInt {
			out[code] = cur
		}
	}
	return out, nil
}

type propReader struct {
	d   []byte
	o   int
	err error
}

func (r *propReader) take(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.d)-r.o) {
		r.err = fmt.Errorf("truncated at offset %d", r.o)
		return nil
	}
	b := r.d[r.o : r.o+int(n)]
	r.o += int(n)
	return b
}

func (r *propReader) u8() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *propReader) u16() uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

// PTP data type codes 1..8: int8, uint8, int16, uint16, ... uint64.
func intSize(typ uint16) uint64 {
	if typ >= 1 && typ <= 8 {
		return 1 << ((typ - 1) / 2)
	}
	return 0
}

// value reads one value of PTP type typ; isInt reports a scalar integer.
func (r *propReader) value(typ uint16) (v int64, isInt bool) {
	switch {
	case intSize(typ) > 0:
		b := r.take(intSize(typ))
		if b == nil {
			return 0, false
		}
		var u uint64
		for i := len(b) - 1; i >= 0; i-- {
			u = u<<8 | uint64(b[i])
		}
		if typ%2 == 1 { // signed: sign-extend
			shift := 64 - 8*len(b)
			return int64(u<<shift) >> shift, true
		}
		return int64(u), true
	case typ == 0xFFFF: // string: u8 length in UTF-16 units
		r.take(2 * uint64(r.u8()))
	case typ&0x4000 != 0 && intSize(typ&0xFF) > 0: // array: u32 count
		var cnt uint64
		if b := r.take(4); b != nil {
			cnt = uint64(binary.LittleEndian.Uint32(b))
		}
		r.take(cnt * intSize(typ&0xFF))
	default:
		if r.err == nil {
			r.err = fmt.Errorf("unknown type %#04x at offset %d", typ, r.o)
		}
	}
	return 0, false
}

// ------------------------------------------------------------------ discovery

func hostCacheFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, appID, "camera-host")
}

func portOpen(host string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(ptpPort)), timeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// discover scans the /24 of every local IPv4 interface for the PTP/IP port.
func discover() string {
	// "Ctrl w/ Smartphone" mode: the computer is on the camera's own Wi-Fi,
	// where the camera always has this address.
	if portOpen(smartphoneModeHost, 700*time.Millisecond) {
		return smartphoneModeHost
	}
	if f := hostCacheFile(); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			if h := strings.TrimSpace(string(b)); h != "" && portOpen(h, time.Second) {
				return h
			}
		}
	}
	addrs, _ := net.InterfaceAddrs()
	var cands []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		ip := ipn.IP.To4()
		if ip[0] == 169 && ip[1] == 254 { // link-local
			continue
		}
		for i := 1; i < 255; i++ {
			if byte(i) == ip[3] {
				continue
			}
			cands = append(cands, fmt.Sprintf("%d.%d.%d.%d", ip[0], ip[1], ip[2], i))
		}
	}
	found := make(chan string, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 128)
	for _, c := range cands {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if portOpen(c, 700*time.Millisecond) {
				found <- c
			}
		}(c)
	}
	go func() { wg.Wait(); close(found) }()
	h, ok := <-found
	if !ok {
		return ""
	}
	if f := hostCacheFile(); f != "" {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte(h), 0o644)
	}
	return h
}

func cameraLoop(h *hub, fixedHost string, logProps bool) {
	c := &controller{applied: -1, logProps: logProps} // lives across reconnects (keeps the original focus area)
	// While the camera refuses us (not paired yet), keep retrying that same
	// camera and keep the "refused" stage on screen instead of going back to
	// searching, so the setup guide stays on the pairing step.
	refusedBy := ""
	for {
		host := fixedHost
		if host == "" && refusedBy != "" && portOpen(refusedBy, time.Second) {
			host = refusedBy
		}
		refused := host != "" && host == refusedBy
		refusedBy = ""
		if host == "" {
			h.setPhase("searching", "Looking for the camera on the network…")
			host = discover()
			if host == "" {
				h.setPhase("notfound", "Camera not found on this network yet.")
				time.Sleep(3 * time.Second)
				continue
			}
		}
		if !refused {
			h.setPhase("connecting", "Connecting to camera at "+host+"…")
		}
		// The camera holds its answer while it shows a pairing / OK prompt.
		prompt := time.AfterFunc(2*time.Second, func() {
			h.setPhase("confirm", "The camera is asking for confirmation: press OK on the camera.")
		})
		p, err := dial(host)
		prompt.Stop()
		if err == nil {
			time.Sleep(time.Second) // Sony bodies want a pause after connecting
			err = p.sonyHandshake()
			if err == nil {
				var props map[uint16]int64
				if props, err = p.readProps(); err == nil {
					c.connected(h, props)
					h.setStatus("Waiting for live view…")
					err = p.stream(h, c)
				}
			}
			p.close()
		}
		if errors.Is(err, errRefused) {
			h.setPhase("refused", "The camera refused the connection: start Pairing on the camera.")
			refusedBy = host
			time.Sleep(3 * time.Second)
			continue
		}
		log.Println("camera connection:", err)
		h.setPhase("lost", "Can't reach the camera, retrying…")
		time.Sleep(2 * time.Second)
	}
}

// ------------------------------------------------------------------ HTTP / window

// activity tracks page requests, to quit once the window is gone.
type activity struct {
	mu     sync.Mutex
	active int // requests in progress (long polls)
	last   time.Time
	ever   bool
}

func (a *activity) touch(d int) {
	a.mu.Lock()
	a.active += d
	a.ever = true
	a.last = time.Now()
	a.mu.Unlock()
}

func (a *activity) idle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ever && a.active == 0 && time.Since(a.last) > idleTimeout
}

// MIME types required by CONTRACT.md, independent of the system's mime.types.
var mimeTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".mjs":  "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".json": "application/json",
	".wasm": "application/wasm",
	".onnx": "application/octet-stream",
}

func serve(h *hub, ln net.Listener, act *activity, web fs.FS) {
	mux := http.NewServeMux()
	static := http.StripPrefix("/web/", http.FileServerFS(web))
	mux.HandleFunc("GET /web/", func(w http.ResponseWriter, r *http.Request) {
		if t, ok := mimeTypes[path.Ext(r.URL.Path)]; ok {
			w.Header().Set("Content-Type", t)
		}
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(web, "index.html")
		if err != nil {
			http.Error(w, "index.html missing from build", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		act.touch(0)
		h.mu.Lock()
		st := struct {
			Status    string `json:"status"`
			Phase     string `json:"phase"`
			Battery   *int   `json:"battery"`
			Recording bool   `json:"recording"`
			RecSince  *int64 `json:"recSince"`
			Tracking  bool   `json:"tracking"`
		}{Status: h.status, Phase: h.phase, Recording: h.recording, Tracking: h.tracking}
		if h.battery >= 0 {
			b := h.battery
			st.Battery = &b
		}
		if h.recSince != 0 {
			t := h.recSince
			st.RecSince = &t
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /record", func(w http.ResponseWriter, r *http.Request) {
		act.touch(0)
		var req struct{ On *bool }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.On == nil {
			http.Error(w, `want {"on":true|false}`, http.StatusBadRequest)
			return
		}
		h.requestRecord(*req.On)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /frame", func(w http.ResponseWriter, r *http.Request) {
		act.touch(1)
		defer act.touch(-1)
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		frame, seq := h.waitFrame(r.Context(), after, frameWait)
		w.Header().Set("Cache-Control", "no-store")
		if frame == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Seq", strconv.FormatUint(seq, 10))
		w.Header().Set("Content-Length", strconv.Itoa(len(frame)))
		w.Write(frame)
	})
	mux.HandleFunc("POST /af", func(w http.ResponseWriter, r *http.Request) {
		act.touch(0)
		var req struct{ X, Y *float64 }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.X == nil || req.Y == nil {
			http.Error(w, `want {"x":0..1,"y":0..1}`, http.StatusBadRequest)
			return
		}
		h.requestAF(*req.X, *req.Y)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /tracking", func(w http.ResponseWriter, r *http.Request) {
		act.touch(0)
		var req struct{ On *bool }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.On == nil {
			http.Error(w, `want {"on":true|false}`, http.StatusBadRequest)
			return
		}
		h.setTracking(*req.On)
		w.WriteHeader(http.StatusNoContent)
	})
	http.Serve(ln, mux)
}

func openWindow(url string) {
	switch runtime.GOOS {
	case "darwin":
		for _, app := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium"} {
			if _, err := os.Stat("/Applications/" + app + ".app"); err == nil {
				if exec.Command("open", "-na", app, "--args", "--app="+url, "--window-size=1024,606").Run() == nil {
					return
				}
			}
		}
		exec.Command("open", url).Run()
	case "linux":
		for _, b := range []string{"chromium", "google-chrome", "brave"} {
			if path, err := exec.LookPath(b); err == nil {
				exec.Command(path, "--app="+url, "--window-size=1024,606").Start()
				return
			}
		}
		exec.Command("xdg-open", url).Start()
	default:
		fmt.Println("Open", url)
	}
}

func main() {
	host := flag.String("host", "", "camera IP address (default: search the local network)")
	logProps := flag.Bool("logprops", false, "log every camera property change (diagnostics)")
	noWindow := flag.Bool("no-window", false, "do not open a window (and do not exit when idle), just serve http://127.0.0.1:<port>/")
	port := flag.Int("port", 0, "local HTTP port (default: random)")
	flag.Parse()

	if logDir, err := os.UserCacheDir(); err == nil {
		os.MkdirAll(filepath.Join(logDir, appID), 0o755)
		if f, err := os.OpenFile(filepath.Join(logDir, appID, "log.txt"),
			os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/"
	log.Println("serving", url)

	h := newHub()
	act := &activity{}
	logEvents = *logProps
	go cameraLoop(h, *host, *logProps)
	go serve(h, ln, act, web)
	if !*noWindow {
		openWindow(url)
	}

	// On exit, put the user's focus area back before closing.
	quit := func(why string) {
		log.Println(why + ", restoring focus area")
		if h.restoreFocus(restoreWait) {
			log.Println("focus area is at its original value, exiting")
		} else {
			log.Println("focus area not confirmed (camera not connected?), exiting")
		}
		os.Exit(0)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Quit once the window has been closed for a while.
	tick := time.NewTicker(time.Second)
	for {
		select {
		case s := <-sig:
			quit("got " + s.String())
		case <-tick.C:
			if !*noWindow && act.idle() {
				quit("window closed")
			}
		}
	}
}

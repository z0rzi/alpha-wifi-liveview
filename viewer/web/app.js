// Live view + YuNet face tracking that steers the camera's Flexible Spot.
// HTTP API: see viewer/CONTRACT.md.
'use strict';

// Detector input width; the frame is scaled to it keeping the aspect ratio,
// then zero-padded bottom/right to a multiple of 32.
const INPUT_W = 640;
const MODEL_URL = '/web/face_detection_yunet_2026may.onnx';
const SCORE_THRESHOLD = 0.7;
const NMS_IOU = 0.3;
// Detection rate adapts to the machine: wait max(70 ms, 4 × inference time)
// between runs, so detection uses at most ~25% of the page's main thread
// (~11/s on a fast machine, ~4/s on a slow one).
const DETECT_MIN_INTERVAL_MS = 70;
const DETECT_CPU_FACTOR = 4;
const STICKY_IOU = 0.3;          // keep the previous target if it overlaps this much
const EMA_ALPHA = 0.65;          // weight of the new eye position (higher = more responsive)
const AF_INTERVAL_MS = 500;      // at most one POST /af per interval
const AF_DEADBAND = 0.015;       // ...and only if the point moved more than this (fraction of width)

const canvas = document.getElementById('view');
const ctx = canvas.getContext('2d');
const hintEl = document.getElementById('hint');
const statusEl = document.getElementById('status');

// ------------------------------------------------------------------ state

let frame = null;            // latest ImageBitmap
let frameW = 0, frameH = 0;
let session = null;
let detectorError = '';
let busy = false;
let lastDetectStart = -Infinity;
let avgInferMs = 30;          // running average, drives the adaptive rate

let faces = [];              // latest detections, frame pixels
let target = null;           // tracked face (from `faces`) or null
let lastTargetBox = null;    // box of the last tracked face, for stickiness
let point = null;            // smoothed AF point {x, y}, frame pixels
let sentPoint = null;        // last point POSTed to /af, frame pixels
let lastAfPost = -Infinity;

let trackingOn = true;       // what we last asked the server for
let paused = false;          // manual point set by a click
let manualPoint = null;      // normalized {x, y}

// ------------------------------------------------------------------ server

function post(path, body) {
  return fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }).catch(() => {});
}

function setTracking(on) {
  trackingOn = on;
  post('/tracking', { on });
}

function sendAf(nx, ny) {
  const x = Math.min(1, Math.max(0, nx)), y = Math.min(1, Math.max(0, ny));
  lastAfPost = performance.now();
  post('/af', { x: +x.toFixed(4), y: +y.toFixed(4) });
}

let serverStatus = '';
let everStreamed = false;   // after the first picture, only the status line reports problems

// Setup guide, shown until the first frame (and again if the camera refuses us).
const setupEl = document.getElementById('setup');
const SETUP_ACTIVE = {
  searching: ['remote', 'wifi'], notfound: ['remote', 'wifi'],
  connecting: [], confirm: ['pair'], refused: ['pair'], lost: [],
};
function updateSetup(phase) {
  if (phase === 'streaming') everStreamed = true;
  const show = phase !== 'streaming' && (!everStreamed || phase === 'refused') && !!phase;
  setupEl.hidden = !show;
  document.body.classList.toggle('setup', show);
  if (!show) return;
  const active = SETUP_ACTIVE[phase] || [];
  for (const li of setupEl.querySelectorAll('li')) li.classList.toggle('active', active.includes(li.dataset.step));
  document.getElementById('setup-pair-note').textContent =
    phase === 'confirm' ? 'The camera is asking now: press OK on the camera.' :
    phase === 'refused' ? 'The camera refused this app: start Pairing on the camera, then press OK when asked.' : '';
  document.getElementById('setup-status').textContent = serverStatus || 'Waiting for the camera…';
}
let cam = { battery: null, recording: false, recSince: null };
async function pollState() {
  let st = null;
  try {
    const r = await fetch('/state', { cache: 'no-store' });
    st = await r.json();
    serverStatus = (st.status || '').trim();
    cam = { battery: st.battery, recording: !!st.recording, recSince: st.recSince };
  } catch {
    serverStatus = 'Viewer stopped.';
  }
  statusEl.textContent = serverStatus;
  document.body.classList.toggle('has-status', serverStatus !== '');
  updateSetup(st && st.phase);
  if (recPending && (cam.recording === recPending.want || performance.now() > recPending.until)) recPending = null;
  updateBar();
}

async function frameLoop() {
  let seq = 0, timeouts = 0;
  for (;;) {
    try {
      const r = await fetch('/frame?after=' + seq, { cache: 'no-store' });
      if (r.status === 204) {
        // The server may have restarted with a lower sequence number.
        if (++timeouts >= 2) seq = 0;
        continue;
      }
      if (!r.ok) throw new Error('HTTP ' + r.status);
      timeouts = 0;
      const s = +r.headers.get('X-Seq');
      seq = s > 0 ? s : seq + 1;
      const bmp = await createImageBitmap(await r.blob());
      if (frame) frame.close();
      frame = bmp;
      frameW = bmp.width;
      frameH = bmp.height;
      requestDraw();
      maybeDetect();
    } catch {
      await new Promise(res => setTimeout(res, 500));
    }
  }
}

// ------------------------------------------------------------------ detector

async function loadDetector() {
  try {
    ort.env.wasm.wasmPaths = '/web/ort/';
    ort.env.wasm.numThreads = 1;
    session = await ort.InferenceSession.create(MODEL_URL, {
      executionProviders: ['wasm'],
      graphOptimizationLevel: 'all',
    });
  } catch (e) {
    detectorError = 'Face detector failed to load: ' + (e && e.message || e);
    console.error(e);
  }
  updateHint();
}

const prep = { canvas: null, ctx: null, w: 0, h: 0, padW: 0, padH: 0, data: null };

// Scales the frame to INPUT_W and fills a zero-padded BGR float32 NCHW tensor
// with raw 0-255 values (what OpenCV's FaceDetectorYN feeds the model).
function makeInput(bmp) {
  const w = INPUT_W, h = Math.round(bmp.height * INPUT_W / bmp.width);
  const padW = Math.ceil(w / 32) * 32, padH = Math.ceil(h / 32) * 32;
  if (prep.w !== w || prep.h !== h) {
    prep.canvas = new OffscreenCanvas(w, h);
    prep.ctx = prep.canvas.getContext('2d', { willReadFrequently: true });
    prep.ctx.imageSmoothingQuality = 'medium';
    Object.assign(prep, { w, h, padW, padH, data: new Float32Array(3 * padW * padH) });
  }
  prep.ctx.drawImage(bmp, 0, 0, w, h);
  const px = prep.ctx.getImageData(0, 0, w, h).data;
  const plane = padW * padH, out = prep.data;
  for (let y = 0; y < h; y++) {
    let i = y * w * 4, o = y * padW;
    for (let x = 0; x < w; x++, i += 4, o++) {
      out[o] = px[i + 2];             // B
      out[o + plane] = px[i + 1];     // G
      out[o + 2 * plane] = px[i];     // R
    }
  }
  return new ort.Tensor('float32', out, [1, 3, padH, padW]);
}

function iou(a, b) {
  const x1 = Math.max(a[0], b[0]), y1 = Math.max(a[1], b[1]);
  const x2 = Math.min(a[0] + a[2], b[0] + b[2]), y2 = Math.min(a[1] + a[3], b[1] + b[3]);
  const inter = Math.max(0, x2 - x1) * Math.max(0, y2 - y1);
  const uni = a[2] * a[3] + b[2] * b[3] - inter;
  return uni > 0 ? inter / uni : 0;
}

const clamp01 = v => v < 0 ? 0 : v > 1 ? 1 : v;

// Port of OpenCV FaceDetectorYNImpl::postProcess. Returns faces in input pixels:
// {box:[x,y,w,h], kps:[reye x,y, leye x,y, nose x,y, rmouth x,y, lmouth x,y], score}
// (eyes and mouth corners are the subject's right/left).
function decode(out, padW, padH) {
  const cands = [];
  for (const s of [8, 16, 32]) {
    const cls = out['cls_' + s].data, obj = out['obj_' + s].data;
    const bbox = out['bbox_' + s].data, kps = out['kps_' + s].data;
    const cols = padW / s, rows = padH / s;
    for (let r = 0; r < rows; r++) {
      for (let c = 0; c < cols; c++) {
        const idx = r * cols + c;
        const score = Math.sqrt(clamp01(cls[idx]) * clamp01(obj[idx]));
        if (score < SCORE_THRESHOLD) continue;
        const b = idx * 4;
        const cx = (c + bbox[b]) * s, cy = (r + bbox[b + 1]) * s;
        const w = Math.exp(bbox[b + 2]) * s, h = Math.exp(bbox[b + 3]) * s;
        const k = idx * 10, pts = new Array(10);
        for (let n = 0; n < 5; n++) {
          pts[2 * n] = (kps[k + 2 * n] + c) * s;
          pts[2 * n + 1] = (kps[k + 2 * n + 1] + r) * s;
        }
        cands.push({ box: [cx - w / 2, cy - h / 2, w, h], kps: pts, score });
      }
    }
  }
  cands.sort((a, b) => b.score - a.score);
  const keep = [];
  for (const f of cands) {
    if (keep.every(k => iou(k.box, f.box) <= NMS_IOU)) keep.push(f);
  }
  return keep;
}

async function maybeDetect() {
  if (!session || busy || !frame) return;
  const t0 = performance.now();
  const interval = Math.max(DETECT_MIN_INTERVAL_MS, DETECT_CPU_FACTOR * avgInferMs);
  if (t0 - lastDetectStart < interval) return;
  busy = true;
  lastDetectStart = t0;
  try {
    const fw = frameW;
    const input = makeInput(frame);
    const { padW, padH } = prep;
    const out = await session.run({ input });
    const scale = fw / INPUT_W;
    faces = decode(out, padW, padH).map(f => ({
      box: f.box.map(v => v * scale),
      kps: f.kps.map(v => v * scale),
      score: f.score,
    }));
    avgInferMs += 0.2 * (performance.now() - t0 - avgInferMs);
    onDetections();
  } catch (e) {
    console.error('detection failed', e);
  } finally {
    busy = false;
  }
}

// ------------------------------------------------------------------ tracking

function pickTarget() {
  if (!faces.length) return null;
  if (lastTargetBox) {
    let best = null, bestIou = STICKY_IOU;
    for (const f of faces) {
      const v = iou(f.box, lastTargetBox);
      if (v > bestIou) { best = f; bestIou = v; }
    }
    if (best) return best;
  }
  return faces.reduce((a, b) => (b.box[2] * b.box[3] > a.box[2] * a.box[3] ? b : a));
}

// The near eye is the one farther from the nose: when the head turns, the
// far eye moves toward the nose in the image.
function nearEye(f) {
  const k = f.kps;
  if (!k || k.length < 6 || !k.every(Number.isFinite)) {
    return { x: f.box[0] + f.box[2] / 2, y: f.box[1] + f.box[3] / 2 };
  }
  const d0 = Math.hypot(k[0] - k[4], k[1] - k[5]);
  const d1 = Math.hypot(k[2] - k[4], k[3] - k[5]);
  return d0 >= d1 ? { x: k[0], y: k[1] } : { x: k[2], y: k[3] };
}

function onDetections() {
  target = pickTarget();
  if (target) {
    lastTargetBox = target.box;
    const eye = nearEye(target);
    if (!point) point = eye;
    else point = {
      x: point.x + EMA_ALPHA * (eye.x - point.x),
      y: point.y + EMA_ALPHA * (eye.y - point.y),
    };
    if (trackingOn && !paused) {
      const now = performance.now();
      const moved = !sentPoint || Math.hypot(point.x - sentPoint.x, point.y - sentPoint.y) > AF_DEADBAND * frameW;
      if (moved && now - lastAfPost >= AF_INTERVAL_MS) {
        sentPoint = { ...point };
        sendAf(point.x / frameW, point.y / frameH);
      }
    }
  }
  // Face lost: keep `point` where it was (the camera holds the last spot).
  updateHint();
  requestDraw();
}

// ------------------------------------------------------------------ input

function frameRect() {
  const cw = canvas.width, ch = canvas.height;
  if (!frameW) return { x: 0, y: 0, s: 1 };
  const s = Math.min(cw / frameW, ch / frameH);
  return { x: (cw - frameW * s) / 2, y: (ch - frameH * s) / 2, s };
}

canvas.addEventListener('click', e => {
  if (!frameW) return;
  const dpr = window.devicePixelRatio || 1, r = frameRect();
  const x = (e.clientX * dpr - r.x) / r.s / frameW, y = (e.clientY * dpr - r.y) / r.s / frameH;
  if (x < 0 || x > 1 || y < 0 || y > 1) return;
  // The camera only takes a spot position in Flexible Spot, which tracking selects.
  if (!trackingOn) setTracking(true);
  paused = true;
  manualPoint = { x, y };
  sendAf(x, y);
  updateHint();
  requestDraw();
});

window.addEventListener('keydown', e => {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.key === 't' || e.key === 'T') toggleTracking();
  else if (e.key === 'g' || e.key === 'G') cycleGrid();
  else if (e.key === ' ') {
    e.preventDefault();             // no page scroll / native button activation
    if (!e.repeat) toggleRecord();
  }
});
// A focused button would also react to Space on keyup: swallow it.
window.addEventListener('keyup', e => { if (e.key === ' ') e.preventDefault(); });

function toggleTracking() {
  if (paused) {
    paused = false;                 // resume tracking after a manual point
    manualPoint = null;
    sentPoint = null;               // re-send the face point right away
    lastAfPost = -Infinity;
    if (!trackingOn) setTracking(true);
  } else {
    setTracking(!trackingOn);
    if (trackingOn) { sentPoint = null; lastAfPost = -Infinity; }
  }
  updateHint();
  updateBar();
  requestDraw();
}

// ------------------------------------------------------------------ toolbar

const GRIDS = ['off', 'thirds', 'thirds+centre', 'square'];
const GRID_LABELS = { off: 'Grid: off', thirds: 'Grid: thirds', 'thirds+centre': 'Grid: thirds + centre', square: 'Grid: square' };
let grid = GRIDS.includes(localStorage.getItem('grid')) ? localStorage.getItem('grid') : 'off';

function cycleGrid() {
  grid = GRIDS[(GRIDS.indexOf(grid) + 1) % GRIDS.length];
  localStorage.setItem('grid', grid);
  updateBar();
  requestDraw();
}

const bar = document.getElementById('bar');
const battEl = document.getElementById('batt');
const battFill = document.getElementById('batt-fill');
const battText = document.getElementById('batt-text');
const recBtn = document.getElementById('rec');
const recTime = document.getElementById('rec-time');
const trackBtn = document.getElementById('track');
const gridBtn = document.getElementById('grid');
let recPending = null;   // {want, until} while waiting for the camera to confirm

function toggleRecord() {
  if (recPending) return;
  const want = !cam.recording;
  recPending = { want, until: performance.now() + 5000 };
  post('/record', { on: want });
  lastMouse = performance.now();   // show the toolbar while it switches
  updateBar();
}
// Buttons give focus back, so Space never "clicks" the last-clicked button.
recBtn.addEventListener('click', () => { recBtn.blur(); toggleRecord(); });
trackBtn.addEventListener('click', () => { trackBtn.blur(); toggleTracking(); });
gridBtn.addEventListener('click', () => { gridBtn.blur(); cycleGrid(); });
// Clicks on the toolbar must not set a focus point.
bar.addEventListener('click', e => e.stopPropagation());

const fmtTime = ms => {
  const t = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(t / 3600), m = Math.floor(t / 60) % 60, s = t % 60;
  return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(s).padStart(2, '0');
};

function updateBar() {
  const b = cam.battery;
  battText.textContent = b == null ? '–' : b + '%';
  battFill.style.width = (b == null ? 0 : Math.max(0, Math.min(100, b))) + '%';
  battEl.classList.toggle('low', b != null && b <= 15);

  const rec = cam.recording;
  recBtn.classList.toggle('on', rec);
  recBtn.disabled = !!recPending;
  recBtn.querySelector('.lbl').textContent = recPending ? (recPending.want ? 'Starting…' : 'Stopping…') : rec ? 'Stop' : 'Record';
  recTime.textContent = rec && cam.recSince ? fmtTime(Date.now() - cam.recSince) : '';
  document.body.classList.toggle('recording', rec);

  trackBtn.querySelector('.lbl').textContent = paused ? 'Tracking: paused' : trackingOn ? 'Tracking: on' : 'Tracking: off';
  trackBtn.classList.toggle('on', trackingOn && !paused);
  gridBtn.querySelector('.lbl').textContent = GRID_LABELS[grid];

  // Auto-hide, except while recording or when the battery is low.
  const pinned = rec || (b != null && b <= 15) || !!recPending;
  bar.classList.toggle('hidden', !pinned && performance.now() - lastMouse > 3000);
}

let lastMouse = performance.now();
window.addEventListener('mousemove', () => { lastMouse = performance.now(); updateBar(); });
setInterval(updateBar, 500);

function drawGrid(r) {
  if (grid === 'off') return;
  const dpr = window.devicePixelRatio || 1;
  const x0 = r.x, y0 = r.y, w = frameW * r.s, h = frameH * r.s;
  ctx.save();
  ctx.strokeStyle = 'rgba(255,255,255,0.35)';
  ctx.lineWidth = 1 * dpr;
  ctx.beginPath();
  const [nx, ny] = grid === 'square' ? [6, 4] : [3, 3];
  for (let i = 1; i < nx; i++) { const x = Math.round(x0 + w * i / nx) + 0.5; ctx.moveTo(x, y0); ctx.lineTo(x, y0 + h); }
  for (let i = 1; i < ny; i++) { const y = Math.round(y0 + h * i / ny) + 0.5; ctx.moveTo(x0, y); ctx.lineTo(x0 + w, y); }
  if (grid === 'thirds+centre') {
    const cx = x0 + w / 2, cy = y0 + h / 2, l = 12 * dpr;
    ctx.moveTo(cx - l, cy); ctx.lineTo(cx + l, cy);
    ctx.moveTo(cx, cy - l); ctx.lineTo(cx, cy + l);
  }
  ctx.stroke();
  ctx.restore();
}

// ------------------------------------------------------------------ drawing

function updateHint() {
  let t;
  if (detectorError) t = detectorError;
  else if (paused) t = 'Manual focus point — T to resume tracking';
  else if (!trackingOn) t = 'Tracking off';
  else if (!session) t = 'Loading face detector…';
  else if (!target) t = 'Tracking on, searching for a face';
  else t = 'Tracking on';
  if (hintEl.textContent !== t) hintEl.textContent = t;
}

let drawPending = false;
function requestDraw() {
  if (drawPending) return;
  drawPending = true;
  requestAnimationFrame(draw);
}

function resize() {
  const dpr = window.devicePixelRatio || 1;
  const w = Math.round(innerWidth * dpr), h = Math.round(innerHeight * dpr);
  if (canvas.width !== w || canvas.height !== h) {
    canvas.width = w;
    canvas.height = h;
  }
}
window.addEventListener('resize', requestDraw);

function corners(x, y, w, h, len) {
  ctx.beginPath();
  ctx.moveTo(x, y + len); ctx.lineTo(x, y); ctx.lineTo(x + len, y);
  ctx.moveTo(x + w - len, y); ctx.lineTo(x + w, y); ctx.lineTo(x + w, y + len);
  ctx.moveTo(x + w, y + h - len); ctx.lineTo(x + w, y + h); ctx.lineTo(x + w - len, y + h);
  ctx.moveTo(x + len, y + h); ctx.lineTo(x, y + h); ctx.lineTo(x, y + h - len);
  ctx.stroke();
}

function draw() {
  drawPending = false;
  resize();
  ctx.fillStyle = '#000';
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  if (!frame) return;
  const r = frameRect(), dpr = window.devicePixelRatio || 1;
  ctx.drawImage(frame, r.x, r.y, frameW * r.s, frameH * r.s);
  const X = v => r.x + v * r.s, Y = v => r.y + v * r.s;
  const tracking = trackingOn && !paused;
  drawGrid(r);

  ctx.lineWidth = 1.5 * dpr;
  for (const f of faces) {
    if (f === target && tracking) continue;
    ctx.strokeStyle = 'rgba(255,255,255,0.3)';
    ctx.strokeRect(X(f.box[0]), Y(f.box[1]), f.box[2] * r.s, f.box[3] * r.s);
  }
  if (tracking && target) {
    const [bx, by, bw, bh] = target.box;
    ctx.strokeStyle = 'rgba(255,214,10,0.9)';
    ctx.lineWidth = 2 * dpr;
    corners(X(bx), Y(by), bw * r.s, bh * r.s, Math.min(bw, bh) * r.s * 0.22);
  }
  if (tracking && point) {
    ctx.beginPath();
    ctx.arc(X(point.x), Y(point.y), 4 * dpr, 0, 2 * Math.PI);
    if (target) {
      ctx.fillStyle = 'rgba(255,214,10,0.95)';
      ctx.fill();
    } else {               // searching: hollow dot at the held point
      ctx.strokeStyle = 'rgba(255,214,10,0.6)';
      ctx.lineWidth = 1.5 * dpr;
      ctx.stroke();
    }
  }
  if (paused && manualPoint) {
    const x = X(manualPoint.x * frameW), y = Y(manualPoint.y * frameH), s = 10 * dpr;
    ctx.strokeStyle = 'rgba(255,255,255,0.9)';
    ctx.lineWidth = 1.5 * dpr;
    ctx.strokeRect(x - s, y - s, 2 * s, 2 * s);
  }
}

// ------------------------------------------------------------------ start

setTracking(true);
updateHint();
updateBar();
pollState();
setInterval(pollState, 1000);
loadDetector();
frameLoop();

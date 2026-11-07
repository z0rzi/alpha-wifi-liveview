# alpha-wifi-liveview

A small live-view window for Sony Alpha cameras over Wi-Fi. The computer
detects faces in the feed and moves the camera's focus point onto them. I
wanted something lighter than Sony's Imaging Edge Desktop "Remote" for the
common case: see the feed, frame yourself, record.

Tested with a Sony A7C (ILCE-7C). Other bodies are listed under
[Compatibility](#compatibility).

What it does:

- Shows the live view over Wi-Fi (1024×576, about 14 fps) in a bare window.
- Tracks faces. The camera turns off its own Face/Eye AF in PC Remote mode,
  so the app runs a face detector
  ([YuNet](https://github.com/opencv/opencv_zoo/tree/main/models/face_detection_yunet))
  on the feed and keeps the camera's Flexible Spot on the nearer eye. The
  camera's phase-detect AF still does the focusing.
- Focuses wherever you click.
- Starts and stops movie recording, shows the battery level, and draws
  framing grids.
- Walks you through the camera setup until the camera connects.
- Ships as one binary with nothing to install: a universal macOS `.app`, or a
  Linux executable. The window is a browser window in app mode.

> Not affiliated with or endorsed by Sony. "Sony", "Alpha" and "Imaging Edge"
> are trademarks of Sony Group Corporation.

## Camera setup

Do this once:

1. MENU → Network → PC Remote Function: set PC Remote to On and PC Remote
   Cnct Method to Wi-Fi Access Point.
2. MENU → Network → Wi-Fi Settings → Access Point Set.: join the same Wi-Fi
   network as the computer.
3. MENU → Network → PC Remote Function → Pairing, then start the app. When
   the camera asks to connect to "Alpha Live View", press OK.

After that, turn the camera on with PC Remote enabled and start the app. It
looks for the camera on the local network (it scans the local /24 for TCP
port 15740), and shows these steps until it gets a connection.

Smartphone mode works too. Start Ctrl w/ Smartphone on the camera and join
its `DIRECT-xxxx` Wi-Fi from the computer. The picture drops to 640×360, and
the computer has no internet while connected.

## Use

| Key | Action |
|---|---|
| Space | start or stop movie recording on the camera |
| T | turn face tracking on or off (or resume it after a manual point) |
| G | cycle the grid: off, thirds, thirds with a centre cross, square |
| Click | focus on that point (pauses tracking) |

The toolbar at the bottom right hides itself until you move the mouse. It
stays up while recording or when the battery is low. Closing the window quits
the app and puts the camera's focus area back to what it was.

The log is in `~/Library/Caches/alpha-wifi-liveview/log.txt` on macOS and
`~/.cache/alpha-wifi-liveview/log.txt` on Linux.

## Build

You need Go 1.22 or later and curl. `fetch-assets.sh` downloads the
third-party web assets (onnxruntime-web WASM and the YuNet model, both MIT),
checks their sha256 sums, and the build embeds them in the binary:

```sh
cd viewer
./fetch-assets.sh
go build -o alpha-wifi-liveview .
./alpha-wifi-liveview    # opens the window (Chrome/Chromium/Brave app mode, or default browser)
```

Flags: `-host <ip>` skips discovery, `-no-window` only serves
`http://127.0.0.1:<port>/`, and `-logprops` logs camera property changes and
PTP events for debugging.

To build the macOS app (from Linux or macOS, with `llvm-lipo` and `zip`):

```sh
viewer/build-mac.sh      # → viewer/build/Alpha Live View.app and AlphaLiveView.zip
```

The app has only an ad-hoc signature. If you downloaded or AirDropped it,
the first launch needs System Settings → Privacy & Security → Open Anyway;
copying it over with `scp` avoids the quarantine flag. macOS may also ask for
Local Network access. Allow it, or the app can't find the camera.

## How it works

```
camera ──PTP/IP (TCP 15740)──▶ Go app ──HTTP (127.0.0.1)──▶ page (canvas + YuNet in WASM)
                                  ▲                               │
                                  └──── AF point (POST /af) ◀─────┘
```

The camera speaks PTP/IP with Sony's SDIO extension, the same protocol
Imaging Edge uses over Wi-Fi.

- The app opens the init command and event channels with a fixed GUID. That
  GUID is what the camera remembers when you pair.
- Handshake: `OpenSession`, then `0x9201` SDIO_Connect phases 1 and 2,
  `0x9202` GetExtDeviceInfo (protocol `0x12C`), and `0x9201` phase 3.
- Live view comes from `GetObject(0xFFFFC002)`. The blob starts with a u32
  offset (136) and the u32 size of the JPEG.
- `0x9209` returns the full property list on every call (battery `0xD218`,
  focus area `0xD22C`, and so on).
- `0x9205` changes a setting, with the value in a data phase.
- `0x9207` presses buttons. The AF point is `0xD2DC` with `x<<16 | y` on a
  640×480 grid; Movie Rec is `0xD2C8` (press 2, release 1).

Face tracking runs in the page. YuNet (`2026may`, dynamic input) runs on
onnxruntime-web as single-threaded WASM and takes about 25 ms per frame at
640×384. The detection rate adapts to the machine so it stays under about a
quarter of one core. The page follows the largest face, sticking with it from
frame to frame, and aims at the eye farther from the nose, which is the eye
nearer the camera. It smooths that point and sends it at most every 500 ms.

[`viewer/CONTRACT.md`](viewer/CONTRACT.md) documents the HTTP API between the
page and the app.

## Compatibility

| Camera | Status |
|---|---|
| A7C (ILCE-7C) | tested |
| A9 II, A7R IV/IVA, A1, A6600, ZV-E10, … | probably works: same PTP/IP + SDIO protocol generation, untested |
| A7 IV, A7R V, A7C II, A6700, FX3, ZV-E1, … | not yet: these need "Access Authentication" (an SSH tunnel) |
| Older bodies with only the smartphone HTTP API | no |

If you try it on another body, please report how it went.

Two limits come from Sony's firmware, not from this app. Face/Eye AF is
greyed out while PC Remote is on, which is why the app tracks faces itself.
Over the smartphone remote, the XAVC S 4K file format blocks it.

## License

MIT, see [LICENSE](LICENSE). The downloaded assets keep their own licenses:
onnxruntime-web (MIT, Microsoft) and YuNet (MIT, OpenCV Zoo).

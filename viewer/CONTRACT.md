# HTTP contract between the Go app and the web page

All on `http://127.0.0.1:<port>`, same origin.

| Method | Path | Request | Response |
|---|---|---|---|
| GET | `/` | – | `web/index.html` |
| GET | `/web/...` | – | static files embedded from `viewer/web/` (`.mjs` → `text/javascript`, `.wasm` → `application/wasm`, `.onnx` → `application/octet-stream`) |
| GET | `/frame?after=N` | `N` = last seq seen (0 initially) | blocks until a frame with seq > N exists (max 5 s). 200 `image/jpeg` with header `X-Seq: <seq>` and `Cache-Control: no-store`; 204 on timeout. |
| POST | `/af` | JSON `{"x":0..1,"y":0..1}` normalized to the full frame (0,0 top-left) | 204. Newest request wins; Go throttles to ≥60 ms between camera commands. |
| POST | `/tracking` | JSON `{"on":true\|false}` | 204. `true` → camera Focus Area = Flexible Spot M (0x0102); `false` → restore the user's original focus area. Desired state is remembered and re-applied after reconnect. |
| GET | `/state` | – | JSON `{"status":"…","phase":"searching\|notfound\|connecting\|confirm\|refused\|lost\|streaming","battery":0..100\|null,"recording":bool,"recSince":unix ms\|null,"tracking":bool}`. Polled every second. |
| POST | `/record` | JSON `{"on":true\|false}` | 204. Presses the camera's Movie Rec button if the known recording state differs from `on`. |

Liveness: the page polls `/state` every second and long-polls `/frame`
continuously; the Go app exits (restoring the focus area) when no request
has arrived for 8 s after the first one.

Frames are 1024×576 in PC Remote mode, 640×360 in smartphone mode; the page
uses the decoded image's actual size.

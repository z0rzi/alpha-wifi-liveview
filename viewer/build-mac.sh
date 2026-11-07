#!/usr/bin/env bash
# Build "Alpha Live View.app" (universal arm64 + x86_64) and a zip of it, from Linux.
# Usage: nix-shell -p go llvmPackages.bintools-unwrapped zip --run ./build-mac.sh
set -euo pipefail
cd "$(dirname "$0")"
OUT=build
APP="$OUT/Alpha Live View.app"
rm -rf "$OUT" && mkdir -p "$APP/Contents/MacOS"
./fetch-assets.sh   # third-party web assets (embedded into the binary)

for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "$OUT/bin-$arch" .
done
# Each slice keeps the ad-hoc signature added by the Go linker.
llvm-lipo -create "$OUT/bin-arm64" "$OUT/bin-amd64" -output "$APP/Contents/MacOS/alpha-wifi-liveview"
chmod +x "$APP/Contents/MacOS/alpha-wifi-liveview"

cat > "$APP/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Alpha Live View</string>
  <key>CFBundleDisplayName</key><string>Alpha Live View</string>
  <key>CFBundleIdentifier</key><string>io.github.z0rzi.alpha-wifi-liveview</string>
  <key>CFBundleExecutable</key><string>alpha-wifi-liveview</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>LSUIElement</key><true/>
  <key>NSLocalNetworkUsageDescription</key><string>Alpha Live View connects to your Sony camera on the local network to show its live view.</string>
</dict>
</plist>
EOF

(cd "$OUT" && rm -f AlphaLiveView.zip && zip -qry AlphaLiveView.zip "Alpha Live View.app")
rm -f "$OUT"/bin-arm64 "$OUT"/bin-amd64
ls -la "$OUT"

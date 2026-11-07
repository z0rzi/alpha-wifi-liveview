#!/usr/bin/env bash
# Download the third-party files the web page needs into viewer/web/.
# Idempotent: files already present with the pinned sha256 are kept.
# Needs only curl, tar and sha256sum (or shasum on macOS).
set -euo pipefail

cd "$(dirname "$0")"
WEB=web
ORT_VERSION=1.31.1
MODEL=face_detection_yunet_2026may.onnx
MODEL_URL="https://media.githubusercontent.com/media/opencv/opencv_zoo/main/models/face_detection_yunet/$MODEL"
ORT_URL="https://registry.npmjs.org/onnxruntime-web/-/onnxruntime-web-$ORT_VERSION.tgz"

MODEL_SHA=ebafce4e3c118d6554634be5c27ab333b4c047a9a8c3faf1d7cf93101c22f0f0
ORT_TGZ_SHA=bdccdb9fbb9ab537247c0c734e9b1d2b1b338b4f2a09faa58f0dc1cfa27c312c
# file in package/dist/  sha256
ORT_FILES="
ort.wasm.min.js              b8a78c408c40da65fdc8ba0846f7b2948e92a4c6d6b23cc8e4165e428ad95443
ort-wasm-simd-threaded.mjs   30ed0b5cd1182ede931fea448afb567d4dc705ec957af09f108a2be5f770dac3
ort-wasm-simd-threaded.wasm  4b884c203b0fa48d057be3332c65513ff85aa3c5bddd6946acc0fab7ecda001d
"

sha() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
ok() { [ -f "$1" ] && [ "$(sha "$1")" = "$2" ]; }
check() {
  local got; got=$(sha "$1")
  if [ "$got" != "$2" ]; then
    echo "sha256 mismatch for $1: got $got, want $2" >&2
    exit 1
  fi
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$WEB/ort"

if ok "$WEB/$MODEL" "$MODEL_SHA"; then
  echo "ok   $WEB/$MODEL"
else
  echo "get  $MODEL"
  curl -fsSL -o "$TMP/$MODEL" "$MODEL_URL"
  check "$TMP/$MODEL" "$MODEL_SHA"
  mv "$TMP/$MODEL" "$WEB/$MODEL"
fi

need_ort=0
while read -r f h; do
  [ -z "$f" ] && continue
  ok "$WEB/ort/$f" "$h" || need_ort=1
done <<<"$ORT_FILES"

if [ $need_ort = 0 ]; then
  echo "ok   $WEB/ort/ (onnxruntime-web $ORT_VERSION)"
else
  echo "get  onnxruntime-web $ORT_VERSION"
  curl -fsSL -o "$TMP/ort.tgz" "$ORT_URL"
  check "$TMP/ort.tgz" "$ORT_TGZ_SHA"
  while read -r f h; do
    [ -z "$f" ] && continue
    tar -xzf "$TMP/ort.tgz" -C "$TMP" "package/dist/$f"
    check "$TMP/package/dist/$f" "$h"
    mv "$TMP/package/dist/$f" "$WEB/ort/$f"
  done <<<"$ORT_FILES"
fi
echo "assets ready in $(pwd)/$WEB"

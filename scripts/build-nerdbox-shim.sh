#!/bin/sh
# Build the nerdbox shim with raw-virtio multi-layer support. The upstream
# gptLayerThreshold=8 path produces a GPT VMDK that libkrun 1.19.6 rejects;
# the patch keeps up to 128 plain EROFS layers on raw virtio disks. The proxy
# channel patch adds a guest-dialed vsock port bridged to a host unix socket.
set -eu
: "${NERDBOX_SRC:?set NERDBOX_SRC to an unpacked nerdbox v0.2.5 tree}"
: "${OUT:?set OUT to the shim output path}"
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
"$ROOT/scripts/verify-pinned-source.sh" "$NERDBOX_SRC" 28c86e8e16c62a08079531ebe99e24a7bdad3d62
WORK=$(mktemp -d "${TMPDIR:-/tmp}/klorq-nerdbox-build.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
cp -R "$NERDBOX_SRC" "$WORK/src"
chmod -R u+w "$WORK/src"
"$ROOT/scripts/verify-pinned-source.sh" "$WORK/src" 28c86e8e16c62a08079531ebe99e24a7bdad3d62
cd "$WORK/src"
git apply "$ROOT/build/patches/nerdbox-gpt-threshold.patch"
git apply "$ROOT/build/patches/nerdbox-proxy-channel.patch"
CGO_ENABLED=1 \
CGO_CFLAGS="-I${LIBKRUN_PREFIX:-/opt/homebrew/opt/libkrun}/include" \
CGO_LDFLAGS="-L${LIBKRUN_PREFIX:-/opt/homebrew/opt/libkrun}/lib -lkrun" \
GOWORK=off go build -o "$WORK/shim" ./cmd/containerd-shim-nerdbox-v1
codesign --force --sign - \
  --entitlements cmd/containerd-shim-nerdbox-v1/containerd-shim-nerdbox-v1.entitlements \
  "$WORK/shim"
mkdir -p "$(dirname -- "$OUT")"
cp "$WORK/shim" "$OUT"
chmod 0755 "$OUT"
shasum -a 256 "$OUT"

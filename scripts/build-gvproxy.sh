#!/bin/sh
# Build the policy-enforcing gvproxy from a pinned upstream checkout.
set -eu
: "${GVPROXY_SRC:?set GVPROXY_SRC to the pinned gvisor-tap-vsock source tree}"
: "${OUT:?set OUT to the gvproxy output path}"
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
"$ROOT/scripts/verify-pinned-source.sh" "$GVPROXY_SRC" 0b0819c174331f5b86d96db3ab7aad7dd126078e
WORK=$(mktemp -d "${TMPDIR:-/tmp}/lickren-gvproxy-build.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
cp -R "$GVPROXY_SRC" "$WORK/src"
chmod -R u+w "$WORK/src"
"$ROOT/scripts/verify-pinned-source.sh" "$WORK/src" 0b0819c174331f5b86d96db3ab7aad7dd126078e
cd "$WORK/src"
git apply "$ROOT/build/patches/gvproxy-egress-policy-v2.patch"
GOWORK=off go build -trimpath -buildvcs=false -ldflags=-buildid= -o "$WORK/gvproxy" ./cmd/gvproxy
mkdir -p "$(dirname -- "$OUT")"
cp "$WORK/gvproxy" "$OUT"
chmod 0755 "$OUT"
shasum -a 256 "$OUT"

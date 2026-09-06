#!/usr/bin/env bash
# Build the certification probe for the device it is going to run on
# (issue #89, D2.1).
#
# One binary per architecture, and that is not a convenience. `hwcert
# record` stamps the evidence record's probe.goarch from its own
# runtime.GOARCH, so the architecture a record claims is decided by which
# binary the operator ran rather than by what anybody typed. Ship the amd64
# probe to an amd64 device and the arm64 probe to an arm64 device; ship the
# wrong one and it refuses rather than records.
#
#   scripts/hwcert/build-probe.sh                  # both, into dist/hwcert/
#   scripts/hwcert/build-probe.sh --arch arm64     # just one
#
# Static, CGO off, so it runs on a NAS with whatever libc UGOS ships.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

OUT_DIR=dist/hwcert
ARCHES="amd64 arm64"

while [ $# -gt 0 ]; do
  case "$1" in
    --arch) ARCHES="$2"; shift 2 ;;
    --out) OUT_DIR="$2"; shift 2 ;;
    -h|--help)
      echo "usage: $0 [--arch amd64|arm64] [--out DIR]"
      exit 0 ;;
    *) echo "build-probe: unknown option $1" >&2; exit 2 ;;
  esac
done

version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
mkdir -p "$OUT_DIR"

for arch in $ARCHES; do
  case "$arch" in
    amd64|arm64) ;;
    *) echo "build-probe: $arch is not an architecture this release claims" >&2; exit 2 ;;
  esac
  out="$OUT_DIR/hwcert-linux-$arch"
  echo "==> $out"
  (cd distribution && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "-X main.version=$version" -o "../$out" ./cmd/hwcert)
done

echo
echo "Copy the probe for the device's architecture onto the NAS, alongside"
echo "scripts/hwcert/measure-ugos.sh and docs/acceptance/ugos-resource-certification.md."
echo "Nothing else has to go with them, and no credential ever does."

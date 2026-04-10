#!/usr/bin/env bash
# Build traffic-gen and failure-runner as native host binaries (no local Go needed).
# Detects host OS/arch and cross-compiles via a Go container.
#
# Usage:
#   ./scripts/build.sh                    # auto-detect runtime (docker > podman)
#   CONTAINER_RUNTIME=podman ./scripts/build.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
mkdir -p "$BIN"

# Auto-detect runtime: prefer docker, fall back to podman.
if [ -z "${CONTAINER_RUNTIME:-}" ]; then
  if command -v docker >/dev/null 2>&1; then
    CONTAINER_RUNTIME=docker
  elif command -v podman >/dev/null 2>&1; then
    CONTAINER_RUNTIME=podman
  else
    echo "ERROR: neither docker nor podman found" >&2
    exit 1
  fi
fi

GO_IMAGE="golang:1.26"

# Detect host platform for cross-compilation.
HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64)        GOARCH=amd64 ;;
  arm64|aarch64) GOARCH=arm64 ;;
  *)             GOARCH="$(uname -m)" ;;
esac
GOOS="$HOST_OS"

echo "==> Building for $GOOS/$GOARCH  runtime=$CONTAINER_RUNTIME  image=$GO_IMAGE"
echo ""

build_binary() {
  local module_dir="$1"
  local binary="$2"
  echo "--> $module_dir → bin/$binary"
  "$CONTAINER_RUNTIME" run --rm \
    -v "$ROOT:/src" \
    -w "/src/$module_dir" \
    -e GOPATH=/tmp/go \
    -e GOOS="$GOOS" \
    -e GOARCH="$GOARCH" \
    -e CGO_ENABLED=0 \
    "$GO_IMAGE" \
    go build -o "/src/bin/$binary" .
}

build_binary "cmd/traffic-gen"    "traffic-gen"
build_binary "cmd/failure-runner" "failure-runner"

echo ""
echo "Done:"
ls -lh "$BIN/"

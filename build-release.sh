#!/bin/bash
# PanDrive universal build script
# Builds frontend once, embeds it, cross-compiles backend for all targets.
# Usage: ./build-release.sh [version]
set -euo pipefail

VERSION="${1:-dev}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/backend-go/release"
cd "$ROOT/frontend"

echo "==> Building frontend..."
npm run build

echo "==> Copying dist into backend-go for embedding..."
rm -rf "$ROOT/backend-go/dist"
cp -r "$ROOT/frontend/dist" "$ROOT/backend-go/dist"

cd "$ROOT/backend-go"
echo "==> Cross compiling (version: $VERSION)..."
mkdir -p "$OUT"

LDFLAGS="-s -w -X main.buildVersion=$VERSION"

build() {
  local GOOS="$1" GOARCH="$2" SUFFIX="$3"
  echo "    -> pandrive-${GOOS}-${GOARCH}${SUFFIX}"
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/pandrive-${GOOS}-${GOARCH}${SUFFIX}" .
}

build windows amd64 .exe
build windows arm64 .exe
build linux   amd64 ""
build linux   arm64 ""
build darwin  amd64 ""
build darwin  arm64 ""

echo "==> Cleaning embedded dist (gitignored, only for build)..."
rm -rf "$ROOT/backend-go/dist"

echo "==> Done. Artifacts in: $OUT"
ls -lh "$OUT"

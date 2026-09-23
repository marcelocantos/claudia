#!/usr/bin/env bash
# Build release tarballs into dist/ (darwin/arm64 + linux/amd64 + linux/arm64).
set -euo pipefail
source "$(dirname "$0")/release-common.sh"

VERSION="$(release_version)"
DIST="$ROOT/dist"
BUILD="$DIST/build"

rm -rf "$DIST"
mkdir -p "$BUILD"

build_one() {
	local goos=$1 goarch=$2 cgo=$3 asset_os=$4 asset_arch=$5
	local out asset

	out="$BUILD/claudia"
	asset="claudia-${VERSION}-${asset_os}-${asset_arch}.tar.gz"

	CGO_ENABLED="$cgo" GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/claudia
	tar -czf "$DIST/$asset" -C "$BUILD" claudia -C "$ROOT" LICENSE README.md THIRD_PARTY_NOTICES
	rm -f "$out"
	echo "wrote dist/$asset"
}

build_one darwin arm64 0 darwin arm64
build_one linux amd64 0 linux amd64
build_one linux arm64 0 linux arm64

echo "release-package: v${VERSION} → dist/"
ls -1 "$DIST"

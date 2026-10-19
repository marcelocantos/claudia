#!/usr/bin/env bash
# Build release tarballs into dist/ (darwin/arm64 + linux/amd64 + linux/arm64).
set -euo pipefail
source "$(dirname "$0")/release-common.sh"

VERSION="$(release_version)"
DIST="$ROOT/dist"
BUILD="$DIST/build"

rm -rf "$DIST"
mkdir -p "$BUILD"

# 🎯T157: the broker runs its plan seats in a Bun sidecar, so a release that
# ships only the binary cannot start one. The sidecar's sources, manifest and
# lockfile ride along; its dependencies are installed on first start (T156),
# because pi-natives' native addon is ~170 MB per platform.
#
# Paths keep their place in the tree: package.json names its dependency
# patches as patches/<name>.patch, and a flattened copy left bun unable to find
# them, so first start could not install the sidecar (v0.54.0–v0.59.0). Tests
# and their fixtures stay behind; no runtime file imports testdata/.
stage_sidecar() {
	mkdir -p "$BUILD/sidecar"
	git -C "$ROOT" ls-files sidecar | grep -v '\.test\.ts$' | grep -v '^sidecar/testdata/' | while read -r f; do
		mkdir -p "$BUILD/$(dirname "$f")"
		cp "$ROOT/$f" "$BUILD/$f"
	done
}
stage_sidecar

# The staged sidecar must install exactly as the broker will install it on
# first start, patches included. A sidecar that cannot install starts no
# plan seat, and nothing else in the release would notice.
verify_sidecar_install() {
	local probe
	probe="$(mktemp -d)"
	cp -R "$BUILD/sidecar" "$probe/"
	if ! (cd "$probe/sidecar" && bun install --frozen-lockfile >"$probe/install.log" 2>&1); then
		echo "release-package: the staged sidecar does not install:" >&2
		cat "$probe/install.log" >&2
		rm -rf "$probe"
		exit 1
	fi
	rm -rf "$probe"
	echo "staged sidecar installs (bun install --frozen-lockfile)"
}
command -v bun >/dev/null || { echo "release-package: bun is required to verify the staged sidecar" >&2; exit 1; }
verify_sidecar_install

# A release binary is built from this module's go.mod alone. An enclosing
# go.work (the sibling workspace on the dev Mac) would add its own modules to
# version selection and could ship different dependency versions.
export GOWORK=off

# The control plane links sqlpipe, which is cgo (C++ over SQLite), so every
# target needs cgo. darwin/arm64 builds natively. The linux targets cross-
# compile with zig as the C and C++ toolchain against musl and link
# statically, so the binary runs on any linux of that architecture.
command -v zig >/dev/null || {
	echo "release-package: zig is required to cross-compile the cgo linux targets (brew install zig)" >&2
	exit 1
}

build_one() {
	local goos=$1 goarch=$2 zigtarget=$3 asset_os=$4 asset_arch=$5
	local out asset ldflags

	out="$BUILD/claudia"
	asset="claudia-${VERSION}-${asset_os}-${asset_arch}.tar.gz"

	if [ -z "$zigtarget" ]; then
		CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" \
			go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/claudia
	else
		ldflags="-s -w -linkmode external -extldflags -static"
		CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" \
			CC="zig cc -target $zigtarget" CXX="zig c++ -target $zigtarget" \
			go build -trimpath -ldflags="$ldflags" -o "$out" ./cmd/claudia
	fi
	tar -czf "$DIST/$asset" -C "$BUILD" claudia sidecar -C "$ROOT" LICENSE README.md NOTICE
	rm -f "$out"
	echo "wrote dist/$asset"
}

build_one darwin arm64 "" darwin arm64
build_one linux amd64 x86_64-linux-musl linux amd64
build_one linux arm64 aarch64-linux-musl linux arm64

echo "release-package: v${VERSION} → dist/"
ls -1 "$DIST"

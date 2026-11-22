#!/bin/bash
# Run the analysis's Go reproductions in a scratch copy of the module.
# Usage: run_go_repros.sh <scratch-dir>
# <scratch-dir>/mod must hold `git archive` of this commit. The test files are
# copied in under their package directories; the repository is not touched.
# Private GOCACHE under <scratch-dir>; no network (GOPROXY=off).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
scratch="$1"
mod="$scratch/mod"
cp "$here/daemon_analysis_test.go.txt" "$mod/daemon/zz_analysis_test.go"
cp "$here/client_analysis_test.go.txt" "$mod/zz_analysis_test.go"
export GOCACHE="$scratch/gocache" GOWORK=off GOPROXY=off GOFLAGS=-mod=mod GOMAXPROCS=2
export TMPDIR="$scratch/tmp"
mkdir -p "$TMPDIR"
cd "$mod"
rc=0
go test -count=1 -v -run 'TestAnalysis' ./daemon || rc=$?
go test -count=1 -v -run 'TestAnalysis' . || rc=$?
exit "$rc"

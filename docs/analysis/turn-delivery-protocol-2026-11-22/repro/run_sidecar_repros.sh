#!/bin/bash
# Run the analysis's sidecar reproductions against a scratch copy of sidecar/.
# Usage: run_sidecar_repros.sh <scratch-dir> [test-file-glob]
# <scratch-dir>/sidecar must already hold a copy of this commit's sidecar/
# with node_modules installed (the report says how it was made). Nothing in
# the repository is modified; fixtures run under <scratch-dir>/tmp.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
scratch="$1"
pattern="${2:-analysis-*.test.ts}"
mkdir -p "$scratch/tmp"
for f in "$here"/$pattern; do
  cp "$f" "$scratch/sidecar/"
done
cp "$here"/analysis-*-provider.ts "$scratch/sidecar/testdata/"
cd "$scratch/sidecar"
export TMPDIR="$scratch/tmp"
exec bun test --timeout 200000 $(cd "$here" && ls $pattern)

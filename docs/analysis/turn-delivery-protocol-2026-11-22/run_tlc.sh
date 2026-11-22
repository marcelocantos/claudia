#!/bin/bash
# Check each TLC config against the outcome its first line declares
# ("\* EXPECT: PASS" or "\* EXPECT: <invariant name>").
# Usage: run_tlc.sh <scratch-dir> <cfg>...
# Two TLC workers, fixed seed. Metadata and logs go under <scratch-dir>.
# Exit status is non-zero when any config's outcome differs from its claim.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
scratch="$1"; shift
jar="${TLA2TOOLS_JAR:-$HOME/.local/lib/tla2tools.jar}"
mkdir -p "$scratch/tlc"
mismatch=0
for cfg in "$@"; do
  name="$(basename "$cfg" .cfg)"
  spec="${name%%-*}.tla"
  expect="$(sed -n '1s/^\\\* EXPECT: //p' "$cfg")"
  log="$scratch/tlc/$name.log"
  rc=0
  (cd "$scratch/tlc" && java -Xmx6g -XX:+UseParallelGC -cp "$jar" tlc2.TLC \
      -workers 2 -seed 1 -cleanup -metadir "$scratch/tlc/meta-$name" \
      -config "$here/$cfg" "$here/$spec") > "$log" 2>&1 || rc=$?
  if grep -q 'Model checking completed. No error has been found' "$log"; then
    got=PASS
  elif got="$(sed -n 's/^Error: Invariant \([A-Za-z0-9_]*\) is violated.*/\1/p' "$log" | head -n 1)" && [[ -n "$got" ]]; then
    :
  else
    got="ERROR(rc=$rc)"
  fi
  states="$(sed -n 's/^\([0-9]*\) states generated, \([0-9]*\) distinct states found.*/\1 generated, \2 distinct/p' "$log" | tail -n 1)"
  depth="$(sed -n 's/^The depth of the complete state graph search is \([0-9]*\).*/depth \1/p' "$log" | tail -n 1)"
  if [[ "$got" == "$expect" ]]; then verdict=OK; else verdict=MISMATCH; mismatch=1; fi
  echo "$verdict $name expect=$expect got=$got ($states${depth:+, $depth}) log=$log"
done
exit "$mismatch"

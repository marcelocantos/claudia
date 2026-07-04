#!/bin/sh
# Run TLC on a spec + config using the tla2tools.jar fetched to ~/.local/lib
# (override with TLA2TOOLS_JAR). Usage:
#   scripts/tlc.sh <module.tla> <config.cfg> [extra tlc args...]
set -eu

JAR="${TLA2TOOLS_JAR:-$HOME/.local/lib/tla2tools.jar}"
if [ ! -f "$JAR" ]; then
	echo "tla2tools.jar not found at $JAR" >&2
	echo "fetch it with:" >&2
	echo "  mkdir -p \"$(dirname "$JAR")\" && curl -fsSL -o \"$JAR\" \\" >&2
	echo "    https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar" >&2
	exit 2
fi

MODULE="$1"
CONFIG="$2"
shift 2

cd "$(dirname "$0")/../specs"
# -deadlock: the lifecycle model has legitimate terminal states (all agents
#            reaped); we check safety invariants, not deadlock-freedom.
# -workers 1: deterministic, reproducible runs.
exec java -XX:+UseParallelGC -cp "$JAR" tlc2.TLC \
	-deadlock -workers 1 -config "$CONFIG" "$@" "$MODULE"

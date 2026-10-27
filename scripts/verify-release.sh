#!/usr/bin/env bash
# shellcheck disable=SC2015 # pass/fail/skip always return 0, so A && pass || fail is exact
# verify-release: prove the installed Homebrew claudia is the release it claims
# to be and that the supervised broker is actually running it.
#
# Read-only against the owner's broker: it runs `broker status` and
# `broker usage` and nothing else there. It never creates a seat, never
# restarts or stops the supervised program, and never refreshes a plan login.
# The full exercise (sidecar start, a real Task round trip) runs on a private
# broker started from the same brew binary with its state and both sockets
# under a mktemp dir (XDG_STATE_HOME + CLAUDIA_BROKER_SOCKET +
# CLAUDIA_OMP_SOCKET), CLAUDIA_OMP_NO_REFRESH=1 so it can never rotate the
# shared plan refresh tokens, and --no-resume. It is stopped by exact PID.
#
# Knobs (environment or make variables):
#   VERSION       expected release, e.g. 0.65.0 or v0.65.0 (default: latest vX.Y.Z tag)
#   MIN_GO        minimum Go the binary was built with (default: local `go env GOVERSION`)
#   UPTIME_MIN    seconds the supervised program must have been up (default: 30)
#   OLLAMA_MODEL  model for the private Task round trip (default: first local
#                 completion-capable model; SKIP when ollama is absent)
#   OLLAMA_ENDPOINT  default http://127.0.0.1:11434
#
# Exit status: 0 when nothing FAILed (SKIPs allowed), 1 otherwise.
set -uo pipefail

ROOT="$(CDPATH='' cd "$(dirname "$0")/.." && pwd)"
PROGRAM=claudia
UPTIME_MIN="${UPTIME_MIN:-30}"
OLLAMA_ENDPOINT="${OLLAMA_ENDPOINT:-http://127.0.0.1:11434}"
OLLAMA_ENDPOINT="${OLLAMA_ENDPOINT%/}"

PASSES=() FAILS=() SKIPS=()
pass() { PASSES+=("$1"); printf 'PASS  %s\n' "$1"; }
fail() { FAILS+=("$1"); printf 'FAIL  %s\n' "$1"; }
skip() { SKIPS+=("$1"); printf 'SKIP  %s\n' "$1"; }
note() { printf '      %s\n' "$*"; }

# The owner's broker, reached exactly as any consumer would: no isolate env.
live() { env -u CLAUDIA_BROKER_SOCKET -u CLAUDIA_OMP_SOCKET -u XDG_STATE_HOME "$BIN" "$@"; }

# version_ge A B: A >= B for dotted numeric versions (go prefix ignored).
version_ge() {
	local a="${1#go}" b="${2#go}"
	a="${a%%[^0-9.]*}" b="${b%%[^0-9.]*}"
	[ "$(printf '%s\n%s\n' "$b" "$a" | sort -t. -k1,1n -k2,2n -k3,3n | head -1)" = "$b" ]
}

# supervisord uptime "H:MM:SS" or "N day(s), H:MM:SS" -> seconds.
uptime_seconds() {
	local s="$1" days=0 h m sec
	if [[ "$s" =~ ([0-9]+)\ days?,\ (.*) ]]; then days="${BASH_REMATCH[1]}" s="${BASH_REMATCH[2]}"; fi
	IFS=: read -r h m sec <<<"$s"
	echo $(( days*86400 + 10#$h*3600 + 10#$m*60 + 10#$sec ))
}

pid_exe() { # the mapped executable of a pid, symlinks resolved
	local exe
	exe="$(lsof -a -p "$1" -d txt -Fn 2>/dev/null | sed -n 's/^n//p' | head -1)"
	[ -n "$exe" ] || exe="$(ps -o comm= -p "$1" 2>/dev/null)"
	[ -n "$exe" ] && realpath "$exe" 2>/dev/null || echo "$exe"
}

# ---------------------------------------------------------------- binary
echo "== release binary"
if [ -z "${VERSION:-}" ]; then
	VERSION="$(git -C "$ROOT" tag --list 'v[0-9]*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)"
fi
VERSION="v${VERSION#v}"
WANT="${VERSION#v}"
if [ -z "${MIN_GO:-}" ]; then MIN_GO="$(go env GOVERSION 2>/dev/null || true)"; fi
MIN_GO="${MIN_GO:-go$(sed -n 's/^go //p' "$ROOT/go.mod")}"
note "expect $VERSION, built with >= $MIN_GO"

PREFIX="$(brew --prefix 2>/dev/null || true)"
BIN="$PREFIX/opt/claudia/bin/claudia"
CELLAR_BIN="$PREFIX/Cellar/claudia/$WANT/bin/claudia"
if [ -z "$PREFIX" ] || [ ! -x "$BIN" ]; then
	fail "brew binary present at \$(brew --prefix)/opt/claudia/bin/claudia"
	BIN=""
else
	real="$(realpath "$BIN")"
	if [ "$real" = "$CELLAR_BIN" ]; then pass "brew opt link -> $CELLAR_BIN"
	else fail "brew opt link -> $real (want $CELLAR_BIN)"; fi

	info="$(go version -m "$BIN" 2>&1)"
	mod="$(awk '$1=="mod"{print $3; exit}' <<<"$info")"
	modified="$(awk '$1=="build" && $2 ~ /^vcs.modified=/{sub(/^vcs.modified=/,"",$2); print $2; exit}' <<<"$info")"
	rev="$(awk '$1=="build" && $2 ~ /^vcs.revision=/{sub(/^vcs.revision=/,"",$2); print $2; exit}' <<<"$info")"
	gov="$(head -1 <<<"$info" | awk '{print $NF}')"
	[ "$mod" = "$VERSION" ] && pass "go version -m: module version $mod" || fail "go version -m: module version '${mod:-none}' (want $VERSION)"
	[ "$modified" = "false" ] && pass "go version -m: vcs.modified=false" || fail "go version -m: vcs.modified='${modified:-absent}' (want false)"
	if version_ge "$gov" "$MIN_GO"; then pass "go version -m: built with $gov (>= $MIN_GO)"
	else fail "go version -m: built with ${gov:-unknown} (want >= $MIN_GO)"; fi
	tagrev="$(git -C "$ROOT" rev-parse -q --verify "$VERSION^{commit}" 2>/dev/null || true)"
	if [ -z "$tagrev" ]; then note "tag $VERSION not in this checkout; vcs.revision=$rev not cross-checked"
	elif [ "$rev" = "$tagrev" ]; then pass "go version -m: vcs.revision ${rev:0:12} is tag $VERSION"
	else fail "go version -m: vcs.revision ${rev:-none} is not tag $VERSION ($tagrev)"; fi

	cli="$("$BIN" --version 2>&1)"
	[ "$cli" = "$WANT" ] && pass "claudia --version: $cli" || fail "claudia --version: '$cli' (want $WANT)"
fi

# ---------------------------------------------------------------- supervised broker
echo "== supervised broker (read-only)"
st="$(supervisorctl status "$PROGRAM" 2>&1)"
note "$st"
if [[ "$st" =~ ^${PROGRAM}[[:space:]]+RUNNING[[:space:]]+pid\ ([0-9]+),\ uptime\ (.+)$ ]]; then
	LIVE_PID="${BASH_REMATCH[1]}"
	up="$(uptime_seconds "${BASH_REMATCH[2]}")"
	pass "supervisord $PROGRAM RUNNING (pid $LIVE_PID)"
	[ "$up" -ge "$UPTIME_MIN" ] && pass "uptime ${up}s >= ${UPTIME_MIN}s" || fail "uptime ${up}s < ${UPTIME_MIN}s (just restarted or flapping)"
	exe="$(pid_exe "$LIVE_PID")"
	[ "$exe" = "$CELLAR_BIN" ] && pass "pid $LIVE_PID executable is $exe" || fail "pid $LIVE_PID executable is '${exe:-unknown}' (want $CELLAR_BIN)"
else
	fail "supervisord $PROGRAM RUNNING"
fi

if [ -n "$BIN" ]; then
	if out="$(live broker status 2>&1)" && grep -q '^protocol v[0-9]' <<<"$out"; then
		pass "live broker status: $(head -1 <<<"$out")"
	else
		fail "live broker status"; note "$(head -5 <<<"$out")"
	fi
	if out="$(live broker usage --json 2>&1)" && jq -e '(.providers|type=="array" and length>0) and (.fetched_at|type=="string")' >/dev/null 2>&1 <<<"$out"; then
		pass "live broker usage: $(jq -r '[.providers[]|"\(.provider)=\(.status)"]|join(" ")' <<<"$out")"
	else
		fail "live broker usage --json"; note "$(head -5 <<<"$out")"
	fi
fi

# ---------------------------------------------------------------- private broker
echo "== private broker from the brew binary"
if [ -z "$BIN" ]; then
	skip "private broker (no brew binary)"
else
	# /tmp, not $TMPDIR: macOS TMPDIR is deep enough that broker.sock.control
	# overruns the 104-byte sun_path limit (bind: invalid argument).
	ISO="$(mktemp -d /tmp/claudia-vr.XXXXXX)"
	ISO="$(realpath "$ISO")"
	PRIV_PID="" SIDE_PID=""
	cleanup() {
		if [ -n "$PRIV_PID" ] && kill -0 "$PRIV_PID" 2>/dev/null; then
			kill -TERM "$PRIV_PID" 2>/dev/null
			for _ in $(seq 1 100); do kill -0 "$PRIV_PID" 2>/dev/null || break; sleep 0.2; done
			kill -0 "$PRIV_PID" 2>/dev/null && kill -KILL "$PRIV_PID" 2>/dev/null
		fi
		# The sidecar exits with its broker (lifeline). Only if this run's own
		# sidecar outlived it, stop that exact pid.
		if [ -n "$SIDE_PID" ] && kill -0 "$SIDE_PID" 2>/dev/null && ps -o command= -p "$SIDE_PID" | grep -qF "$ISO/omp.sock"; then
			sleep 2
			kill -0 "$SIDE_PID" 2>/dev/null && kill -TERM "$SIDE_PID" 2>/dev/null
		fi
		[ -n "${KEEP_ISO:-}" ] || rm -rf "$ISO"
	}
	trap cleanup EXIT
	trap 'exit 130' INT TERM

	mkdir -p "$ISO/state" "$ISO/work" "$ISO/spool"
	PRIV_ENV=(env -u CLAUDIA_OMP_SERVER -u CLAUDIA_OMP_AUTH -u CLAUDIA_BIN
		XDG_STATE_HOME="$ISO/state"
		CLAUDIA_BROKER_SOCKET="$ISO/broker.sock"
		CLAUDIA_OMP_SOCKET="$ISO/omp.sock"
		CLAUDIA_OMP_SPOOL_DIR="$ISO/spool"
		CLAUDIA_OMP_NO_REFRESH=1
		CLAUDIA_NO_BROKER=0
		PATH="$HOME/.bun/bin:$PATH")
	priv() { "${PRIV_ENV[@]}" "$@"; }
	kc_mdat() { security find-generic-password -s claudia-plan-credentials 2>/dev/null | sed -n 's/.*"mdat"<timedate>=\(.*\)/\1/p'; }
	kc_before="$(kc_mdat)"

	(cd "$ISO/work" && exec "${PRIV_ENV[@]}" "$BIN" broker serve --no-resume --restart-nudge - >"$ISO/serve.log" 2>&1) &
	PRIV_PID=$!
	note "pid $PRIV_PID, root $ISO"
	up=""
	for _ in $(seq 1 600); do # 120s: a first start may install the packaged sidecar's deps
		kill -0 "$PRIV_PID" 2>/dev/null || break
		if grep -q 'claudia broker serving' "$ISO/serve.log" 2>/dev/null; then up=1; break; fi
		sleep 0.2
	done
	if [ -z "$up" ]; then
		fail "private broker serving"; note "$(tail -20 "$ISO/serve.log")"
	else
		pass "private broker serving on $ISO/broker.sock"
		exe="$(pid_exe "$PRIV_PID")"
		[ "$exe" = "$CELLAR_BIN" ] && pass "private broker executable is $exe" || fail "private broker executable is '${exe:-unknown}' (want $CELLAR_BIN)"
		SIDE_PID="$(cat "$ISO/omp.sock.pid" 2>/dev/null || true)"
		if grep -q 'omp sidecar listening' "$ISO/serve.log" && [ -S "$ISO/omp.sock" ] && [ -n "$SIDE_PID" ] && kill -0 "$SIDE_PID" 2>/dev/null; then
			side_cmd="$(ps -o command= -p "$SIDE_PID")"
			pass "private sidecar up (pid $SIDE_PID)"
			grep -qF "$PREFIX/Cellar/claudia/$WANT/share/claudia/sidecar/server.ts" <<<"$side_cmd" \
				&& pass "private sidecar runs the packaged $WANT sidecar" \
				|| fail "private sidecar script: $side_cmd (want the packaged $WANT sidecar)"
		else
			fail "private sidecar up"; note "$(grep -i sidecar "$ISO/serve.log" | tail -5)"
		fi
		if grep -q 'msg="plan credentials" refreshed=\[\]' "$ISO/serve.log"; then pass "private broker refreshed no plan login"
		else fail "private broker plan refresh line: $(grep -m1 'plan' "$ISO/serve.log")"; fi

		if out="$(priv "$BIN" broker status 2>&1)" && grep -q '^protocol v[0-9]' <<<"$out"; then
			pass "private broker status: $(head -1 <<<"$out")"
			grep -q 'grants 0 ' <<<"$out" && pass "private broker holds no grants" || fail "private broker grants: $(head -1 <<<"$out")"
		else
			fail "private broker status"; note "$(head -5 <<<"$out")"
		fi
		# A fresh private broker has observed nothing: broker reads never start
		# collection, so fetched_at is absent until a scheduled poll lands. It
		# must still list every provider, and none may claim to be available
		# without an observation.
		if out="$(priv "$BIN" broker usage --json 2>&1)" && jq -e '(.providers|type=="array" and length>0) and ((.fetched_at|type=="string") or all(.providers[]; .status != "available"))' >/dev/null 2>&1 <<<"$out"; then
			pass "private broker usage: $(jq -r '[.providers[]|"\(.provider)=\(.status)"]|join(" ")' <<<"$out")"
		else
			fail "private broker usage --json"; note "$(head -5 <<<"$out")"
		fi

		model="${OLLAMA_MODEL:-}"
		if ! tags="$(curl -fsS -m 5 "$OLLAMA_ENDPOINT/api/tags" 2>/dev/null)"; then
			skip "private broker task round trip (no ollama at $OLLAMA_ENDPOINT)"
		else
			if [ -z "$model" ]; then
				# Text-first: a pure vision model (moondream) answers a text-only
				# prompt with nothing, which proves no round trip. Prefer no
				# vision, then tool support, then the smallest.
				model="$(jq -r '[.models[]|(.capabilities//["completion"]) as $c|select($c|index("completion"))
					|{name, k: [($c|index("vision")|type=="number"), ($c|index("tools")|type!="number"), .size]}]
					|sort_by(.k)|.[0].name // empty' <<<"$tags")"
			fi
			if [ -z "$model" ]; then
				skip "private broker task round trip (ollama has no completion model; set OLLAMA_MODEL)"
			else
				t0=$SECONDS
				if out="$(cd "$ISO/work" && priv CLAUDIA_OLLAMA_ENDPOINT="$OLLAMA_ENDPOINT" "$BIN" broker task --provider ollama --model "$model" \
					--workdir "$ISO/work" --timeout 5m --json --prompt 'Reply with the single word PONG.' 2>&1)" \
					&& done="$(grep '^{' <<<"$out" | jq -sr '[.[]|select(.type=="task_done")]|length==1 and (.[0].body.error//"")==""')" \
					&& [ "$done" = true ]; then
					# A clean task_done is not enough: the model must have said something.
					reply="$(grep '^{' <<<"$out" | jq -sr '[.[]|select(.type=="task_event")|.body.event|select(.type=="result")|.content//empty]|last//""')"
					[ -n "$reply" ] || reply="$(grep '^{' <<<"$out" | jq -sr '[.[]|select(.type=="task_event")|.body.event|select(.type=="text")|.content//""]|join("")')"
					if [ -n "$(tr -d '[:space:]' <<<"$reply")" ]; then
						pass "private broker task round trip via ollama/$model in $((SECONDS-t0))s"
						note "reply: $(tr '\n' ' ' <<<"$reply" | head -c 200)"
					else
						fail "private broker task round trip via ollama/$model: task_done but no reply text"; note "$(tail -5 <<<"$out")"
					fi
				else
					fail "private broker task round trip via ollama/$model"; note "$(tail -5 <<<"$out")"
				fi
			fi
		fi
	fi
	kc_after="$(kc_mdat)"
	[ "$kc_before" = "$kc_after" ] && pass "shared plan keychain item unmodified (mdat ${kc_after:-absent})" \
		|| fail "shared plan keychain item changed during the run ($kc_before -> $kc_after)"

	cleanup; trap - EXIT
	if kill -0 "$PRIV_PID" 2>/dev/null; then fail "private broker pid $PRIV_PID stopped"; else pass "private broker pid $PRIV_PID stopped"; fi
	if [ -n "$SIDE_PID" ] && kill -0 "$SIDE_PID" 2>/dev/null; then fail "private sidecar pid $SIDE_PID stopped"; elif [ -n "$SIDE_PID" ]; then pass "private sidecar pid $SIDE_PID stopped"; fi
fi

# ---------------------------------------------------------------- the owner's broker is untouched
if [ -n "${LIVE_PID:-}" ]; then
	now="$(supervisorctl pid "$PROGRAM" 2>/dev/null)"
	[ "$now" = "$LIVE_PID" ] && pass "supervised broker still pid $LIVE_PID" || fail "supervised broker pid changed during the run ($LIVE_PID -> $now)"
fi

echo "== summary: ${#PASSES[@]} PASS, ${#FAILS[@]} FAIL, ${#SKIPS[@]} SKIP"
for f in "${FAILS[@]+"${FAILS[@]}"}"; do echo "FAIL  $f"; done
for s in "${SKIPS[@]+"${SKIPS[@]}"}"; do echo "SKIP  $s"; done
[ "${#FAILS[@]}" -eq 0 ]

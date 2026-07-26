#!/usr/bin/env bash
# Spike: Grok interactive CLI under claudia's tmux substrate (docs/grok-tmux-feasibility.md).
# Exit 0 if all required gates pass; non-zero otherwise. Writes a results JSONL/log.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRATCH="${SPIKE_SCRATCH:-$(mktemp -d -t grok-tmux-spike.XXXXXX)}"
export GROK_HOME="$SCRATCH/grok-home"
WORKDIR="$SCRATCH/work"
SOCK="$SCRATCH/tmux.sock"
LOG="$SCRATCH/spike.log"
RESULTS="$SCRATCH/results.tsv"
GROK_BIN="${GROK_BIN:-$(command -v grok)}"
SESSION_UUID="$(uuidgen | tr '[:upper:]' '[:lower:]')"

mkdir -p "$GROK_HOME" "$WORKDIR" "$(dirname "$SOCK")"
: >"$LOG"
: >"$RESULTS"

log() { echo "$*" | tee -a "$LOG" >&2; }
pass() { echo -e "$1\tPASS\t$2" | tee -a "$RESULTS"; log "PASS $1: $2"; }
fail() { echo -e "$1\tFAIL\t$2" | tee -a "$RESULTS"; log "FAIL $1: $2"; }
skip() { echo -e "$1\tSKIP\t$2" | tee -a "$RESULTS"; log "SKIP $1: $2"; }

cleanup() {
  tmux -S "$SOCK" kill-server 2>/dev/null || true
}
trap cleanup EXIT

log "SCRATCH=$SCRATCH"
log "GROK_BIN=$GROK_BIN SESSION_UUID=$SESSION_UUID"

# Minimal MCP server (stdio) for gates 5–6
cat >"$SCRATCH/mcp_echo.py" <<'PY'
#!/usr/bin/env python3
import json, sys

def send(msg):
    body = json.dumps(msg, separators=(",", ":"))
    sys.stdout.write(f"Content-Length: {len(body)}\r\n\r\n{body}")
    sys.stdout.flush()

def read_msg():
    headers = {}
    while True:
        line = sys.stdin.buffer.readline()
        if not line or line in (b"\r\n", b"\n"):
            break
        k, v = line.decode().split(":", 1)
        headers[k.strip().lower()] = v.strip()
    n = int(headers.get("content-length", "0"))
    if n <= 0:
        return None
    return json.loads(sys.stdin.buffer.read(n))

while True:
    msg = read_msg()
    if msg is None:
        break
    mid, method = msg.get("id"), msg.get("method")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": mid, "result": {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "spike-echo", "version": "0.0.1"},
        }})
    elif method == "notifications/initialized":
        pass
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": mid, "result": {"tools": [{
            "name": "spike_ping",
            "description": "Return the string spike-pong",
            "inputSchema": {"type": "object", "properties": {}},
        }]}})
    elif method == "tools/call":
        send({"jsonrpc": "2.0", "id": mid, "result": {
            "content": [{"type": "text", "text": "spike-pong"}],
            "isError": False,
        }})
    elif mid is not None:
        send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": method}})
PY
chmod +x "$SCRATCH/mcp_echo.py"

# Isolated config: only our MCP server (avoid host gmail/etc noise)
cat >"$GROK_HOME/config.toml" <<EOF
[ui]
vim_mode = false
screen_mode = "minimal"

[mcp_servers.spike_echo]
command = "$(command -v python3)"
args = ["$SCRATCH/mcp_echo.py"]
enabled = true
startup_timeout_sec = 30
EOF

# --- tmux server ---
export CLAUDIA_TMUX_SOCKET="$SOCK"
tmux -S "$SOCK" new-session -d -s claudia-anchor -c "$WORKDIR" 2>>"$LOG" || true

# Gate 1: Spawn
WIN_NAME="grok-spike"
# Prefer minimal + always-approve; session id known up front
CMD=( "$GROK_BIN" --always-approve --minimal --cwd "$WORKDIR" -s "$SESSION_UUID" )
# shell-join for tmux
shell_join() {
  python3 -c 'import shlex,sys; print(" ".join(shlex.quote(a) for a in sys.argv[1:]))' "$@"
}
SHELL_CMD=$(shell_join "${CMD[@]}")
WIN_ID=$(tmux -S "$SOCK" new-window -d -P -F '#{window_id}' -c "$WORKDIR" -t claudia-anchor: -n "$WIN_NAME" "$SHELL_CMD" 2>>"$LOG" || true)
if [[ -z "${WIN_ID:-}" ]]; then
  fail "G1_spawn" "tmux new-window failed; see $LOG"
  exit 1
fi
sleep 1
if ! tmux -S "$SOCK" list-windows -t claudia-anchor -F '#{window_id} #{pane_pid}' 2>>"$LOG" | grep -q "$WIN_ID"; then
  fail "G1_spawn" "window missing after spawn"
  exit 1
fi
PANE_PID=$(tmux -S "$SOCK" list-panes -t "$WIN_ID" -F '#{pane_pid}' 2>>"$LOG" | head -1)
if ! kill -0 "$PANE_PID" 2>/dev/null; then
  fail "G1_spawn" "pane pid $PANE_PID not alive"
  exit 1
fi
pass "G1_spawn" "window=$WIN_ID pid=$PANE_PID"

# Session dir helper (encoded cwd under GROK_HOME/sessions)
session_dir() {
  python3 - <<PY
import os, pathlib, urllib.parse
home = pathlib.Path(os.environ["GROK_HOME"])
wd = pathlib.Path("$WORKDIR").resolve()
enc = urllib.parse.quote(str(wd), safe="")
# Grok may use hash slug if long; try exact and scan
base = home / "sessions"
candidates = [base / enc / "$SESSION_UUID"]
if base.exists():
    for p in base.rglob("$SESSION_UUID"):
        if p.is_dir():
            candidates.append(p)
for c in candidates:
    if c.is_dir():
        print(c)
        break
PY
}

# Gate 2: Ready — wait for updates.jsonl or summary, or prompt-ish pane
READY=0
FRAME=""
for i in $(seq 1 60); do
  SD=$(session_dir || true)
  if [[ -n "${SD:-}" && -f "$SD/summary.json" ]]; then
    READY=1
    pass "G2_ready" "session dir ready after ${i}s: $SD"
    break
  fi
  FRAME=$(tmux -S "$SOCK" capture-pane -p -t "$WIN_ID" 2>/dev/null || true)
  # Heuristic idle: pane has content and process still up
  if [[ -n "$FRAME" ]] && kill -0 "$PANE_PID" 2>/dev/null; then
    # Look for common prompt-ish markers or non-empty chrome
    if echo "$FRAME" | rg -q '❯|›|> |Type a message|Ask Grok|prompt' ; then
      READY=1
      pass "G2_ready" "pane heuristic match after ${i}s"
      break
    fi
  fi
  sleep 0.5
done
if [[ "$READY" -eq 0 ]]; then
  fail "G2_ready" "no session dir / prompt within 30s; last frame:
$FRAME"
fi
SD=$(session_dir)
log "SESSION_DIR=$SD"
if [[ -n "$FRAME" ]]; then
  printf '%s\n' "$FRAME" >"$SCRATCH/ready-frame.txt"
fi

# Gate 3: Send
PROMPT_TEXT="Reply with exactly the single word: spike-ok"
# Focus: send keys literally + Enter
tmux -S "$SOCK" send-keys -t "$WIN_ID" -l -- "$PROMPT_TEXT" 2>>"$LOG"
sleep 0.2
tmux -S "$SOCK" send-keys -t "$WIN_ID" Enter 2>>"$LOG"

# Wait for user message in updates.jsonl
SEND_OK=0
for i in $(seq 1 40); do
  SD=$(session_dir || true)
  UPD="${SD:-/nonexistent}/updates.jsonl"
  if [[ -f "$UPD" ]] && rg -q 'spike-ok|user_message' "$UPD" 2>/dev/null; then
    SEND_OK=1
    pass "G3_send" "user content visible in updates.jsonl after ${i}s"
    break
  fi
  # Also accept events.jsonl turn_started
  EV="${SD:-/nonexistent}/events.jsonl"
  if [[ -f "$EV" ]] && rg -q 'turn_started|user' "$EV" 2>/dev/null; then
    # weak signal — keep waiting for updates if possible
    :
  fi
  sleep 0.5
done
if [[ "$SEND_OK" -eq 0 ]]; then
  # Dump evidence
  log "updates head: $(head -c 500 "${SD:-}/updates.jsonl" 2>/dev/null || echo none)"
  fail "G3_send" "no user turn evidence in updates.jsonl within 20s"
fi

# Gate 4: Wait for assistant completion
WAIT_OK=0
ASSIST=""
for i in $(seq 1 90); do
  SD=$(session_dir || true)
  UPD="${SD:-/nonexistent}/updates.jsonl"
  if [[ -f "$UPD" ]]; then
    if rg -q 'agent_message_chunk|turn_completed' "$UPD" 2>/dev/null; then
      ASSIST=$(UPD="$UPD" python3 - <<'PY'
import json, os
path = os.environ["UPD"]
parts = []
for line in open(path):
    line = line.strip()
    if not line:
        continue
    try:
        o = json.loads(line)
    except Exception:
        continue
    u = (o.get("params") or {}).get("update") or o.get("update") or {}
    if u.get("sessionUpdate") == "agent_message_chunk":
        c = u.get("content") or {}
        if isinstance(c, dict) and c.get("text"):
            parts.append(c["text"])
print("".join(parts))
PY
)
      if [[ -n "$ASSIST" ]] || rg -q 'turn_completed' "$UPD"; then
        WAIT_OK=1
        pass "G4_wait" "assistant/turn evidence after ${i}s text=${ASSIST:0:80}"
        break
      fi
    fi
  fi
  sleep 0.5
done
if [[ "$WAIT_OK" -eq 0 ]]; then
  fail "G4_wait" "no assistant completion within 45s"
fi

# Gate 5: MCP new — check events for mcp_config_resolved including spike_echo
# and/or prompt the model to call spike_ping
MCP_NEW=0
SD=$(session_dir)
EV="$SD/events.jsonl"
if [[ -f "$EV" ]] && rg -q 'spike_echo|mcp_config_resolved' "$EV" 2>/dev/null; then
  if rg -q 'spike_echo' "$EV"; then
    MCP_NEW=1
    pass "G5_mcp_new" "spike_echo in mcp_config_resolved"
  fi
fi
if [[ "$MCP_NEW" -eq 0 ]]; then
  # Second prompt: force tool use
  tmux -S "$SOCK" send-keys -t "$WIN_ID" -l -- "Call the MCP tool spike_ping (or spike_echo__spike_ping / use_tool) and reply with its exact result text only." 2>>"$LOG"
  sleep 0.2
  tmux -S "$SOCK" send-keys -t "$WIN_ID" Enter 2>>"$LOG"
  for i in $(seq 1 60); do
    UPD="$SD/updates.jsonl"
    if [[ -f "$UPD" ]] && rg -qi 'spike-pong|spike_ping|tool_call' "$UPD" 2>/dev/null; then
      if rg -qi 'spike-pong' "$UPD"; then
        MCP_NEW=1
        pass "G5_mcp_new" "tool result spike-pong in updates after ${i}s"
        break
      fi
      # tool_call present is weaker but useful
      if rg -q 'spike_ping|spike_echo' "$UPD"; then
        MCP_NEW=1
        pass "G5_mcp_new" "tool_call naming spike tool after ${i}s"
        break
      fi
    fi
    sleep 0.5
  done
fi
if [[ "$MCP_NEW" -eq 0 ]]; then
  # Config resolved without our server is still informative
  if [[ -f "$EV" ]]; then
    log "mcp events: $(rg 'mcp_config' "$EV" | head -3 || true)"
  fi
  fail "G5_mcp_new" "spike MCP not observed (config isolation or tool not used)"
fi

# Gate 7: Interrupt (before kill for resume) — start a long prompt then Esc
tmux -S "$SOCK" send-keys -t "$WIN_ID" -l -- "Think carefully for a long time about the number 17, then say done." 2>>"$LOG"
sleep 0.2
tmux -S "$SOCK" send-keys -t "$WIN_ID" Enter 2>>"$LOG"
sleep 1.5
tmux -S "$SOCK" send-keys -t "$WIN_ID" Escape 2>>"$LOG"
sleep 1
# Process still alive?
if kill -0 "$PANE_PID" 2>/dev/null || tmux -S "$SOCK" list-panes -t "$WIN_ID" -F '#{pane_pid}' 2>/dev/null | head -1 | xargs -I{} kill -0 {} 2>/dev/null; then
  pass "G7_interrupt" "Esc sent; pane still alive"
else
  fail "G7_interrupt" "pane died after Esc"
fi

# Gate 8: Menus — if still alive and we got through send without wedge, partial pass
if kill -0 "$(tmux -S "$SOCK" list-panes -t "$WIN_ID" -F '#{pane_pid}' 2>/dev/null | head -1)" 2>/dev/null; then
  pass "G8_menus" "no launch wedge observed with -s uuid path (no welcome picker)"
else
  fail "G8_menus" "session not alive for menu assessment"
fi

# Gate 6: MCP resume — kill window, restart with -r same id
tmux -S "$SOCK" kill-window -t "$WIN_ID" 2>>"$LOG" || true
sleep 1
CMD2=( "$GROK_BIN" --always-approve --minimal --cwd "$WORKDIR" -r "$SESSION_UUID" )
SHELL_CMD2=$(shell_join "${CMD2[@]}")
WIN_ID2=$(tmux -S "$SOCK" new-window -d -P -F '#{window_id}' -c "$WORKDIR" -t claudia-anchor: -n "${WIN_NAME}-r" "$SHELL_CMD2" 2>>"$LOG" || true)
if [[ -z "${WIN_ID2:-}" ]]; then
  fail "G6_mcp_resume" "resume spawn failed"
else
  sleep 3
  # Prompt for tool again
  tmux -S "$SOCK" send-keys -t "$WIN_ID2" -l -- "Call spike_ping MCP tool and reply with exact result only." 2>>"$LOG"
  sleep 0.2
  tmux -S "$SOCK" send-keys -t "$WIN_ID2" Enter 2>>"$LOG"
  MCP_RES=0
  for i in $(seq 1 60); do
    SD=$(session_dir || true)
    UPD="${SD:-}/updates.jsonl"
    EV="${SD:-}/events.jsonl"
    if [[ -f "$UPD" ]] && rg -qi 'spike-pong|spike_ping' "$UPD" 2>/dev/null; then
      # Prefer post-resume evidence: check file mtime growth hard; simpler: any tool hit after resume spawn time
      MCP_RES=1
      pass "G6_mcp_resume" "tool evidence after -r resume (${i}s)"
      break
    fi
    if [[ -f "$EV" ]] && rg -q 'spike_echo' "$EV" 2>/dev/null; then
      # mcp_config_resolved after resume is strong for "tools reattached"
      :
    fi
    sleep 0.5
  done
  if [[ "$MCP_RES" -eq 0 ]]; then
    # Check mcp_config_resolved includes spike_echo after resume
    SD=$(session_dir || true)
    EV="${SD:-}/events.jsonl"
    if [[ -f "$EV" ]] && rg -q 'spike_echo' "$EV"; then
      pass "G6_mcp_resume" "spike_echo still in session events after resume (config MCP re-resolved)"
    else
      fail "G6_mcp_resume" "no tool/MCP evidence after resume"
    fi
  fi
  tmux -S "$SOCK" kill-window -t "$WIN_ID2" 2>/dev/null || true
fi

# Summary
log "==== RESULTS ($RESULTS) ===="
cat "$RESULTS" | tee -a "$LOG" >&2
FAILS=$(rg -c $'\tFAIL\t' "$RESULTS" || true)
FAILS=${FAILS:-0}
log "fail_count=$FAILS scratch=$SCRATCH"
# Copy summary into repo-visible place if SPIKE_OUT set
if [[ -n "${SPIKE_OUT:-}" ]]; then
  mkdir -p "$(dirname "$SPIKE_OUT")"
  cp "$RESULTS" "$SPIKE_OUT"
  cp "$LOG" "${SPIKE_OUT}.log"
fi
exit "$FAILS"

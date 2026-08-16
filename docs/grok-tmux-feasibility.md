# Grok CLI as a Claude-style tmux Session driver

Status: feasibility study + spike (2026-07-26).  
CLI studied: Grok Build stable (docs under `~/.grok/docs/`, sessions under `~/.grok/sessions/`).

## Why consider this

ACP Session mode (`grok agent stdio`) is a clean control plane. A 2026-07
spike treated **ACP `mcpServers` on `session/load`** as ignored (CLI bug)
and noted that TUI `--resume` reloads MCP from config discovery. Later
(jevons 🎯T58) user-scoped `~/.grok/config.toml` was shown to attach on
ACP load as well; this document is a historical spike, not current policy
(see [grok-acp-session.md](grok-acp-session.md)).

## Executive summary

| Goal | Feasibility | Notes |
| --- | --- | --- |
| Spawn Grok in claudia tmux | High | Same `SpawnWindow` path as Claude |
| Same-id resume + config MCP | High | Main win vs ACP load |
| Send via keys | Medium–high | Enter to send; Esc/Ctrl+C cancel caveats |
| WaitForResponse via log tail | High | `updates.jsonl` is structured |
| Readiness / menus | Medium | Spike must prove capture-pane patterns |
| Per-session MCP without config files | Low | TUI uses config discovery, not ACP params |

**Bottom line:** Feasible and well-motivated if readiness + MCP-after-resume
gates pass. Do not cut over until the spike gates below are green.

## CLI surface (tmux candidate)

```text
grok --always-approve --cwd <dir> [-m <model>] [-s <uuid> | -r <uuid>]
```

| Flag | Role |
| --- | --- |
| `--cwd` | Workdir |
| `-m` | Model |
| `--always-approve` | Unattended tools |
| `-s <uuid>` | **New** session only (must not already exist) |
| `-r <uuid>` | Resume existing session |
| `--no-alt-screen` / `--minimal` | May ease automation (tradeoffs) |
| `--no-plan`, `--no-subagents` | Reduce autonomy noise |

Headless Task mode stays separate: `grok -p … --output-format streaming-json`.

## Storage

```text
~/.grok/sessions/<encoded-cwd>/<session-id>/
  summary.json
  updates.jsonl      # authoritative conversation (resume/restore)
  events.jsonl       # telemetry (incl. mcp_config_resolved)
  …
```

`updates.jsonl` lines are ACP-shaped `session/update` events (`agent_message_chunk`,
`tool_call`, `turn_completed`, …). Prefer tailing this over scraping the TUI
for assistant text.

MCP for TUI/headless: config discovery each process (`~/.grok/config.toml`
`[mcp_servers.*]`, project `.grok/config.toml`, optional compat sources).
Not ACP `mcpServers` params.

## Spike gates

| # | Gate | Pass criterion |
| --- | --- | --- |
| 1 | Spawn | tmux window runs Grok; process stays alive |
| 2 | Ready | Idle prompt detectable within ≤30s (pane or log) without manual keys |
| 3 | Send | Key inject + Enter produces user turn in `updates.jsonl` |
| 4 | Wait | Tail `updates.jsonl` until assistant text + turn complete |
| 5 | MCP new | Known MCP tool usable / visible on first session |
| 6 | MCP resume | Kill window, `grok -r <same-id>`, tools still available |
| 7 | Interrupt | Esc or Ctrl+C ends turn without killing session |
| 8 | Menus | No infinite wedge on fresh/aged launch (or documented auto-dismiss) |

## Comparison

| Concern | ACP (current) | Tmux TUI |
| --- | --- | --- |
| Structured control | Excellent | Keys + log tail |
| MCP same-id resume | Broken on load | Config re-attach |
| Per-session MCP injection | Excellent | Weak (files + trust) |
| Human attach | None | Full |
| Readiness debt | None | Real |

## Recommendation (pre-spike)

If gates **2** and **6** pass: implement `grokTmuxBackend` as default Session for
tooled durable agents; keep Task headless; demote ACP or keep behind a flag.

If **2** fails hard: prefer multi-turn headless (`-p --resume`) over a fragile TUI.

If **6** fails: tmux does not fix the product pain; stay on ACP + rotation.

## Spike results

Runner: `scripts/spike-grok-tmux.sh` (isolated `GROK_HOME`, claudia-style tmux
socket, Grok Build 0.2.112). Date: 2026-07-26.

| Gate | Result | Evidence |
| --- | --- | --- |
| 1 Spawn | **PASS** | tmux window alive (`window=@1`) |
| 2 Ready | **PASS** | Pane heuristic match (~25s) with `--minimal` |
| 3 Send | **PASS** | User content in `updates.jsonl` after key inject + Enter |
| 4 Wait | **PASS** | `agent_message_chunk` text `spike-ok` (~5s after send) |
| 5 MCP new | **PASS** | `spike_echo` in `mcp_config_resolved` (isolated config) |
| 6 MCP resume | **PASS** | Tool evidence after kill + `grok -r <same-uuid>` (~9s) |
| 7 Interrupt | **PASS** | Esc sent; pane still alive |
| 8 Menus | **PASS** | No launch wedge with `-s <uuid>` (no welcome picker) |

**Verdict: VIABLE.** All eight gates passed (`fail_count=0`).

### Spike implications

- Prefer `--minimal` + `--always-approve` + known `-s`/`-r` UUID for embeds.
- Tail `updates.jsonl` for WaitForResponse; do not rely on pane scrape for text.
- MCP-after-resume works on the TUI path (unlike ACP `session/load`).
- Readiness currently a loose pane heuristic — production needs a tighter
  `MatchReady` like Claude’s prompt-box regex, calibrated on captured frames.

### Reproduce

```bash
SPIKE_OUT=docs/spike-grok-tmux-results.tsv ./scripts/spike-grok-tmux.sh
```

## Appendix: Claude readiness comparison

Claude uses capture-pane + prompt-box regex (`─` / `❯` / `─`) and auto-Enter on
stale resume menus (`internal/tmuxagent/readiness.go`). Grok needs its own
frame patterns; do not reuse Claude’s glyph regex blindly.

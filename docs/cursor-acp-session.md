# Cursor ACP Session mode

Status: shipped for Session mode via `ProviderCursor`. Task mode
(`agent --print`) is documented in the oracle map and bound in
`cursor_task.go`.

## Contract

Cursor Session mode does **not** use tmux or the interactive TUI. It runs:

```text
agent --force --trust --approve-mcps acp
```

and speaks [Agent Client Protocol](https://agentclientprotocol.com)
JSON-RPC lines over stdin/stdout. Official docs:
[cursor.com/docs/cli/acp](https://cursor.com/docs/cli/acp).

Binary discovery prefers `CURSOR_BIN`, then `cursor-agent` on `$PATH`,
then `~/.local/bin/agent`. A PATH entry named `agent` is used only if
it is not Grok's `~/.grok/bin/agent` (same filename, different CLI).

Root flags must precede the `acp` mode word. `--model` is accepted
before `acp` when `Config.Model` is set.

### Lifecycle

1. `initialize` — protocolVersion 1, clientInfo claudia, minimal clientCapabilities
2. `notifications/initialized`
3. `authenticate` with `methodId: "cursor_login"`
4. Session open (`session/load` when a SessionID is offered, else `session/new`)
5. `session/prompt` per `Send`
6. `session/update` notifications (`agent_message_chunk` → assistant text; `tool_call*` → progress)
7. Prompt JSON-RPC result → terminal assistant event for `WaitForResponse`
8. `session/cancel` on `Interrupt`
9. Process kill on `Stop`

### The opening prompt is watched (🎯T83)

A `session/prompt` write returning without error means a line reached a
pipe, not that the seat took the work. A mint could answer `session/new`,
swallow the first prompt and then say nothing forever; the prompt id
stayed in flight, so every later `Send` returned `ErrTurnInFlight`, which
reads as *busy*. The seat looked alive and permanently mid-turn and could
only be cleared by destroying it.

So the session's **first** prompt waits for the peer to say anything at
all about it — a thought, a chunk, a tool call, a result. Silence gets the
brief once more on a re-established turn (`session/cancel`, then a fresh
prompt id). Silence twice leaves the seat **idle** and returns
[`ErrCursorPromptStuck`], so a consumer can retry in place instead of
stopping, parking, restarting, killing and reminting.

| | |
| --- | --- |
| What is bounded | Silence before the **first inbound message** of the turn |
| What is **not** bounded | Turn duration — the first message disarms the wait |
| Scope | The opening prompt only; later `Send`s are a plain write |
| On failure | Seat idle, typed error, never `ErrTurnInFlight` |

`cursorPromptSilenceBound` is measured, not chosen. Three real
`cursor-agent` mints answered their first prompt in **14.0s, 14.1s and
18.3s**, and the first thing to arrive is an `agent_thought_chunk`, not
reply text. A bound in the single-digit seconds — the intuitive choice —
would call every healthy mint stuck. The shipped value is 120s, about
6.5x the worst observed: a false positive costs one retry, a false
negative costs a pinned seat.

### Auto-approve

`--force` plus replies to `session/request_permission` keep unattended
embedding workable. Cursor offers hyphenated option ids (`allow-always`,
`allow-once`, `reject-once`). The shared selector prefers `allow-always`
then any `allow-always*` / `allow-always-*`, then once-grants.

Blocking Cursor extensions:

| Method | Unattended reply |
| --- | --- |
| `cursor/ask_question` | `skipped` |
| `cursor/create_plan` | `accepted` |

Notifications (`cursor/update_todos`, `cursor/task`, `cursor/generate_image`)
are published as progress events; no reply.

Rich `fs/*` and `terminal/*` client methods are declined; the agent still
uses its own tools.

### Resume and MCP

Same load policy as Grok:

| Situation | Behaviour |
| --- | --- |
| `SessionID` set | Always `session/load`. `mcpServers` included when configured. |
| Load fails + `RequireResume` | Error. Never `session/new`. |
| Load fails, no `RequireResume` | Fall through to `session/new`. |
| No `SessionID` | `session/new`. |

MCP channels:

| Channel | Role |
| --- | --- |
| ACP `mcpServers` on new/load | Session-scoped list from `Config.MCPServers` / `MCPConfig` |
| User `~/.cursor/mcp.json` | Additive; may still load under exclusive (no strict flag) |
| Exclusive project mcp | `WorkDir/.cursor/mcp.json` with only `Config.MCPServers` |

Cursor exclusive does **not** rewrite `HOME`. An isolated HOME makes
macOS Keychain miss `cursor-user` and hang `authenticate` on a dialog.
Auth stays on the real `agent login` or `CURSOR_API_KEY` / `--api-key`
(the dedicated-key model rather than a hermetic home).

`LoadMCP` reads `~/.cursor/mcp.json` and tags `ProviderCursor`.
Forum reports have claimed ACP `mcpServers` and
even static config were ignored on some CLI builds — live
`CLAUDIA_CURSOR_LIVE` MCP tests are the gate, not the docs.

### Explicit gaps

| Capability | Status |
| --- | --- |
| AttachCommand / tmux | Unsupported |
| Term log / terminal bytes | Unsupported |
| Rewind | Unsupported |
| PermissionMode / DisallowTools / ExtraArgs | Refused rather than dropped |

### Oracles

- Hermetic Session: `testdata/cursor/acp/fake_acp.py` + `TestHermeticCursorSession*`
- Streamed chunk join (🎯T79): `TestHermeticCursorSessionMultiChunkReply`
  (`FAKE_ACP_CHUNKS`) and the pure `TestAppendTurnText`
- Stuck opening prompt (🎯T83): `TestCursorStuckFirstPrompt*`,
  `TestCursorSlowFirstPromptIsNotStuck`, `TestCursorSecondPromptIsNotWatched`
  (`FAKE_ACP_WITHHOLD`, `FAKE_ACP_SWALLOW_FIRST_PROMPT`,
  `FAKE_ACP_PROMPT_DELAY_MS`)
- Hermetic Task: `testdata/cursor/print/fake_print.py` + `TestHermeticCursorTaskRun`
- Resume identity: `TestHermeticCursorLoadFailsClosedWhenRequireResume`,
  `TestHermeticCursorLoadFallsThroughForMintedID`
- Permissions: `TestHermeticCursorHyphenPermissionOptionID`
- Exclusive MCP: `TestMCPExclusiveCursorProjectMCPJourney` (project
  mcp.json, no HOME rewrite)
- Live (optional): `CLAUDIA_CURSOR_LIVE=1` — Task + Session smoke, goal
  journey, MCP mnemo, exclusive round-trip

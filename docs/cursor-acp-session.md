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
- Hermetic Task: `testdata/cursor/print/fake_print.py` + `TestHermeticCursorTaskRun`
- Resume identity: `TestHermeticCursorLoadFailsClosedWhenRequireResume`,
  `TestHermeticCursorLoadFallsThroughForMintedID`
- Permissions: `TestHermeticCursorHyphenPermissionOptionID`
- Exclusive MCP: `TestMCPExclusiveCursorProjectMCPJourney` (project
  mcp.json, no HOME rewrite)
- Live (optional): `CLAUDIA_CURSOR_LIVE=1` — Task + Session smoke, goal
  journey, MCP mnemo, exclusive round-trip

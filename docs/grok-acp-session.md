# Grok ACP Session mode

Status: shipped for Session mode via `ProviderGrok` (see 🎯T7.4 / 🎯T7.5).

## Contract

Grok Session mode does **not** use tmux or the interactive TUI. It runs:

```text
grok agent --always-approve stdio
```

and speaks [Agent Client Protocol](https://agentclientprotocol.com) JSON-RPC
lines over stdin/stdout.

### Lifecycle

1. `initialize` — protocolVersion 1, clientInfo claudia, minimal clientCapabilities  
2. `notifications/initialized`  
3. Session open (see **Resume and MCP** below)  
4. `session/prompt` per `Send`  
5. `session/update` notifications (`agent_message_chunk` → assistant text; `tool_call*` → progress)  
6. Prompt JSON-RPC result → terminal assistant event (`stopReason` → `end_turn`) for `WaitForResponse`  
7. `session/cancel` on `Interrupt`  
8. Process kill on `Stop`

### Auto-approve

`--always-approve` plus `_meta.yoloMode: true` on `session/new` (and load)
plus auto-reply to `session/request_permission` keep unattended embedding
workable.

Permission replies **must** select an `optionId` from the request's offered
`options` list. Prefer `allow_always`, then any `allow_always_*` (e.g.
`allow_always_bash` for `run_terminal_command`), then `allow_once`. Hardcoding
`allow_always` when only bash-scoped options are offered yields Grok's
`unknown permission option for tool run_terminal_command` and stalls fleet
workers.

Rich `fs/*` and `terminal/*` client methods are declined; the agent still
uses its own tools.

### Resume and MCP

**ACP is not the bug.** The [Agent Client Protocol](https://agentclientprotocol.com)
requires clients to pass `mcpServers` on `session/new`, `session/load`, and
`session/resume`, and agents **MUST** reconnect to those servers on load/resume.

**Grok CLI is the bug.** Observed (2026-07-18, jevons): `grok agent stdio`
attaches `mcpServers` on `session/new` but **silently ignores** them on
`session/load`. A resumed session therefore drops host-supplied MCP tools
(e.g. “Tool not found” for jevons MCP).

claudia’s policy (when `Config.MCPConfig` yields a non-empty server list):

| Situation | Behaviour |
| --- | --- |
| MCP configured (any `SessionID` / `RequireResume`) | **Rotate:** skip `session/load`, `session/new` with tools → **new session id**. |
| No MCP, `SessionID` set | Unchanged `session/load` / fail-closed / fall-through rules |

`RequireResume` cannot preserve both the same session id **and** tools until
Grok fixes load; with MCP, claudia prefers tools and returns a new id
(Registry already persists id changes). Hosts that need both durable chat
and tools should own a journal (e.g. jevons `chatlog`) and `Send` a recap
after attach.

### Explicit gaps

| Capability | Status |
| --- | --- |
| AttachCommand / tmux | Unsupported (empty attach string) |
| Term log / terminal bytes | Unsupported |
| Rewind | Unsupported (no private `~/.grok/sessions` rewrite) |
| Pool Acquire | Not wired for Grok ACP (Claude tmux pool only) |
| Same-id resume **with** MCP | Blocked by Grok CLI until load honours `mcpServers` |

### Maturity risk

ACP is a public protocol; Grok also emits `_x.ai/*` notifications that
claudia ignores. Field shapes on `session/request_permission` outcomes may
evolve — live `CLAUDIA_GROK_LIVE` smokes catch breakage.

Private product extensions used by the interactive TUI (not Session mode)
include `x.ai/billing`, `x.ai/auto-topup-rule`, and `x.ai/session/usage` —
see [grok-usage-billing.md](grok-usage-billing.md). claudia must not call
those without a documented, versioned contract.

### Oracles

- Hermetic: `testdata/grok/acp/fake_acp.py` + `TestHermeticGrokSession*`  
- Live (optional): real `grok agent stdio` via installed CLI and auth  

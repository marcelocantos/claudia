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
3. `session/new` with absolute `cwd`, or `session/load` when `Config.SessionID` is set  
4. `session/prompt` per `Send`  
5. `session/update` notifications (`agent_message_chunk` → assistant text; `tool_call*` → progress)  
6. Prompt JSON-RPC result → terminal assistant event (`stopReason` → `end_turn`) for `WaitForResponse`  
7. `session/cancel` on `Interrupt`  
8. Process kill on `Stop`

### Auto-approve

`--always-approve` plus auto-reply to `session/request_permission` keep
unattended embedding workable. Rich `fs/*` and `terminal/*` client methods
are declined; the agent still uses its own tools.

### Explicit gaps

| Capability | Status |
| --- | --- |
| AttachCommand / tmux | Unsupported (empty attach string) |
| Term log / terminal bytes | Unsupported |
| Rewind | Unsupported (no private `~/.grok/sessions` rewrite) |
| Pool Acquire | Not wired for Grok ACP (Claude tmux pool only) |

### Maturity risk

ACP is a public protocol; Grok also emits `_x.ai/*` notifications that
claudia ignores. Field shapes on `session/request_permission` outcomes may
evolve — live `CLAUDIA_GROK_LIVE` smokes catch breakage.

### Oracles

- Hermetic: `testdata/grok/acp/fake_acp.py` + `TestHermeticGrokSession*`  
- Live (optional): real `grok agent stdio` via installed CLI and auth  

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

**Two tool channels, not one.** A 2026-07-18 jevons observation (resumed
overseer toolless) was first read as “Grok ignores `mcpServers` on
`session/load`.” A 2026-07-26 live check (jevons 🎯T58) split that:

| Channel | On `session/load` |
| --- | --- |
| User-scoped `~/.grok/config.toml` | **Attaches.** Proven: same session id, tools callable. |
| ACP request field `mcpServers` (from `Config.MCPConfig`) | Still sent on load. Whether the CLI honours that field is unverified; it is not a reason to skip load. |

claudia’s policy is therefore the same with or without `MCPConfig`:

| Situation | Behaviour |
| --- | --- |
| `SessionID` set | Always `session/load`. `mcpServers` included when `MCPConfig` is set. |
| Load fails + `RequireResume` | Error. Never `session/new` (🎯T35). |
| Load fails, no `RequireResume` | Fall through to `session/new` (unmaterialized / first mint). |
| No `SessionID` | `session/new`. |

Hosts that need durable tools on resume register them user-scoped (jevons:
`ensureGrokMCPServer` → `config.toml`) and do **not** skip load. File-based
`MCPConfig` is the extra session-scoped list on the ACP wire, not a rotate
switch.

### Explicit gaps

| Capability | Status |
| --- | --- |
| AttachCommand / tmux | Unsupported (empty attach string) |
| Term log / terminal bytes | Unsupported |
| Rewind | Unsupported (no private `~/.grok/sessions` rewrite) |
| Pool Acquire | Not wired for Grok ACP (Claude tmux pool only) |
| Same-id resume **with** tools | Load the id. User-scoped `config.toml` tools survive. ACP `mcpServers` are still passed; not a remint trigger. |

### Maturity risk

ACP is a public protocol; Grok also emits `_x.ai/*` notifications that
claudia ignores. Field shapes on `session/request_permission` outcomes may
evolve — live `CLAUDIA_GROK_LIVE` smokes catch breakage.

Private product extensions used by the interactive TUI (not Session mode)
include `x.ai/billing`, `x.ai/auto-topup-rule`, and `x.ai/session/usage` —
see [grok-usage-billing.md](grok-usage-billing.md). claudia must not call
those without a documented, versioned contract.

### Daemon spawn

`Start` with a running broker asks the daemon to spawn the seat.
`StartDirect` spawns it in the caller. Both call the same ACP start.
What differs is the process environment.

A `brew services` daemon (the Colossus 0.44.0 plist) keeps system
directories first on `PATH` and `~/.grok/bin` last, and it does not
include directories an interactive shell adds (`~/.cargo/bin`,
`~/.py/bin`, `~/.den/bin`, `~/.bun/bin`). Helpers the `grok` CLI execs
by name therefore resolve differently from a shell that prepends
`~/.grok/bin`.

Colossus Pimp-1 smoke on 2026-09-26 (brew daemon socket healthy, grok
weekly still 86%, so no turn was burned) failed both entry points on
the same line, in milliseconds:

| Caller | Result |
| --- | --- |
| Library `Register` + `Launch` (`Parent=pimp`, `Purpose=work`, `ProviderGrok`) | `Launch FAIL` after **7ms**: `broker protocol: agent_failed: omp: sidecar said "error", want ready` |
| CLI at `b21cb7d` | same `agent_failed` line, wall time **0s** |

The library log also printed `WARN adopt failed; falling back to launch`
with that same error. That line was a second broker grant: `Launch`
does not adopt, and a provider `agent_failed` is the daemon's answer,
so the registry now returns it once (`TestLaunchReturnsBrokerAgentFailedOnce`).
The grant wire itself is healthy. This is separate from the grant CLI.

`omp` is a Node helper used by **Grok and Cursor** broker seats. Claude
and Codex stay on their vendor paths and do not start it. Its log on
that machine, `~/.local/state/claudia/omp-sidecar.log`, starts with zsh
`(eval):7: command not found: setsid` and then repeats
`Error: write EPIPE` (`node:net`). macOS does not ship a `setsid`
binary. The helper's startup eval fails, the ready handshake returns
the word `error` immediately, and the Node process writes to the pipe
the parent has already closed. `EPIPE` is that closed pipe. A later
Cursor keep-open Launch failed the same way in about 6ms.

Claudia moves existing user tool directories to the front of the
**Grok and Cursor child's** `PATH` (the daemon's own `PATH` stays
system-first). When that PATH has no `setsid`, Claudia inserts
`~/.local/state/claudia/bin/setsid` after those user directories, a
perl shim that calls `setsid(2)`. `~/.grok/bin` stays ahead of the
shim. Claude and Codex children do not get that shim. The
handshake error names the helper, the status word, the log path, and
`brew install util-linux` (a native `setsid` if perl cannot run the
shim) followed by `brew services restart claudia`. When the log is
present, the error quotes its `setsid` and `EPIPE` lines. The handshake
text itself is not produced by claudia.

Verify on a Mac after installing this build:

```bash
brew install util-linux   # provides setsid
brew services restart claudia
# sock is ~/.local/state/claudia/broker.sock when the brew unit is up
```

Then acquire a Grok or Cursor seat through broker `Start` (leave
`CLAUDIA_NO_BROKER` unset). A healthy seat returns a handle with
`DaemonHeld() == true`. Claude and Codex keep-open grants do not go
through omp. A helper that still answers `error` names itself; read
`~/.local/state/claudia/omp-sidecar.log`.

### Oracles

- Hermetic: `testdata/grok/acp/fake_acp.py` + `TestHermeticGrokSession*`  
- Resume identity (🎯T35): `TestHermeticGrokLoadFailsClosedWhenRequireResume`,
  `TestHermeticGrokRequireResumeWithMCPFailsClosed`,
  `TestHermeticGrokRequireResumeWithMCPKeepsSessionID`,
  `TestHermeticGrokLoadFallsThroughForMintedID`,
  `TestHermeticGrokTooledUnmaterializedLoads`,
  `TestHermeticGrokTooledUnmaterializedFallsThrough`  
- Live (optional): real `grok agent stdio` via installed CLI and auth  

# claudia — agents guide

`github.com/marcelocantos/claudia` is a Go library for embedding
Claude Code sessions in your program. If you're helping a user
integrate it, read this whole document first — the design is small
but has non-obvious constraints.

## Pick the right mode

claudia offers two modes. They are not interchangeable; choose based
on the shape of the work.

|                     | Task mode                            | Session mode                            |
|---------------------|--------------------------------------|-----------------------------------------|
| Type                | `claudia.Task`                       | `claudia.Agent`                         |
| Process model       | New `claude` per prompt              | Persistent PTY                          |
| Output              | Structured NDJSON (stream-json)      | JSONL transcript + raw PTY              |
| Use case            | One-shot generation / analysis       | Multi-turn conversations                |
| Cost accounting     | Yes, per prompt via `TaskEvent`      | Cumulative via `Agent.Usage()`          |
| Resume across runs  | Via `TaskConfig.ClaudeID`            | Via `Config.SessionID`                  |

**Default to Task mode.** It's simpler, gives you structured events,
and exposes cost and token accounting. Only use Session mode if the
user explicitly needs persistent state or wants to observe the
transcript live.

### Codex provider (Task mode)

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider:       claudia.ProviderCodex,
    WorkDir:        "/abs/path",
    Model:          "gpt-5-codex",
    SandboxMode:    "workspace-write",
    ApprovalPolicy: "on-request",
})
```

For Codex, `Task.Run` shells out to `codex exec --json`; `TaskConfig.ClaudeID`
still names the resumable provider session id. Do not assume Codex and
Claude flags are semantically identical: `SandboxMode` and
`ApprovalPolicy` are passed as Codex flags, while Claude Session mode
continues to use `PermissionMode` and `DisallowTools`.

Before spawn, claudia runs `PreflightCodexAuth`: it requires ChatGPT
subscription OAuth (`auth_mode=chatgpt` with a non-empty access token in
`~/.codex/auth.json`, overridable via `CLAUDIA_CODEX_AUTH_PATH`) and
fails closed when the path would use API-key / `OPENAI_API_KEY` per-token
billing. See [docs/codex-subscription-spike.md](docs/codex-subscription-spike.md).

Codex persistent Session mode is experimental and currently fails
closed. `Start(claudia.Config{Provider: claudia.ProviderCodex})`
returns `*claudia.CapabilityError` with `Status ==
claudia.CapabilityExperimental`. Codex rewind, tmux attach, and
terminal logs are unsupported until a public Codex app-server contract
proves equivalent behavior; do not implement them by editing private
Codex storage or by driving the Codex TUI in tmux.

### Grok Build CLI provider (Task mode)

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider: claudia.ProviderGrok,
    WorkDir:  "/abs/path",
    Model:    "grok-4", // optional; empty uses Grok Build default
})
```

For Grok Build CLI, `Task.Run` shells out to
`grok -p <prompt> --output-format streaming-json` (with
`--permission-mode bypassPermissions` for unattended runs). Binary
discovery: `GROK_BIN`, then `grok` on `$PATH`, then known installs
including `~/.grok/bin/grok`. Auth is whatever the installed CLI uses
(`grok login` or `XAI_API_KEY`). Resume uses `TaskConfig.ClaudeID`
as the Grok session id with `--resume`.

Headless `streaming-json` maps `text` → `TaskEventText`, terminal
`end.sessionId` → `TaskEventInit` then `TaskEventResult`, and
`error` → `TaskEventError`. Thought deltas are ignored. Tool-use
and cost/usage are not present on this public stream — do not expect
Claude-parity accounting or tool events. SuperGrok weekly limit /
Extra Credits / Auto Top Up (`/usage` in the TUI) is a separate
consumer-billing surface with **no documented API**;
`grok -p "/usage"` is only a model prompt, not the slash command.
Console API team prepaid balance uses the Management API, not the
Grok Build CLI. Details: [docs/grok-usage-billing.md](docs/grok-usage-billing.md).

Grok persistent Session mode uses ACP over `grok agent stdio`:

```go
agent, err := claudia.Start(claudia.Config{
    Provider: claudia.ProviderGrok,
    WorkDir:  "/abs/path",
    Model:    "grok-4", // optional
})
// Send / WaitForResponse / Interrupt / Stop as with Claude Session.
// AttachCommand is empty (no tmux window). Rewind is unsupported.
```

Or via the registry (set `AgentDef.Provider` — Launch forwards it to
`Start`; empty Provider remains Claude):

```go
_ = reg.Register(claudia.AgentDef{
    Name:      "helper",
    WorkDir:   "/abs/path",
    SessionID: uuid.NewString(), // load falls back to session/new if unknown
    Provider:  claudia.ProviderGrok,
    Model:     "grok-4",
    AutoStart: true,
})
agent, err := reg.Launch("helper")
```

Pass `SessionID` to attempt `session/load`. A materialized resume
(`RequireResume`) never mints a replacement session: load failure is an
error, including when `MCPConfig` is set. `MCPConfig` is converted to
ACP `mcpServers` and sent on both new and load; it does not skip load.
Durable tools on resume belong in user-scoped `~/.grok/config.toml`
(the host registers them). Without `RequireResume`, a failed load may
fall through to `session/new`.
Permissions are auto-approved (`--always-approve`). Rewind remains
`CapabilityUnsupported`; do not truncate private Grok session files.

**Do not confuse `ProviderGrok` with package
`github.com/marcelocantos/claudia/grok`.** The latter is a standalone
Realtime voice WebSocket client; it is not the coding-agent harness.

### AWS Bedrock provider (Task mode only, v1)

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider: claudia.ProviderBedrock,
    ID:       "bedrock-1",
    Model:    "anthropic.claude-3-5-sonnet-20241022-v2:0", // or inference profile
})
```

Bedrock is an **API path** (AWS ConverseStream), not the local `claude`
CLI. Credentials: AWS SDK default chain (`AWS_PROFILE`, env keys, SSO,
instance roles). Region: `CLAUDIA_BEDROCK_REGION` else `AWS_REGION` /
`AWS_DEFAULT_REGION`. Model: `TaskConfig.Model` or
`CLAUDIA_BEDROCK_MODEL_ID`.

v1 maps streamed text deltas → `TaskEventText` and a terminal
`TaskEventResult` (token `Usage` when metadata is present; no `CostUSD`).
**Not claimed:** Session/tmux, resume, rewind, tools, permissions, local
binary discovery. `Start(ProviderBedrock)` fails closed with
`CapabilityError`. Work-account setup:
[docs/bedrock-work-account.md](docs/bedrock-work-account.md). Design:
[docs/bedrock-provider.md](docs/bedrock-provider.md).

## Plan usage (subscription remaining + rollover)

Per-run token `Usage` / `CostUSD` on Task events is **not** the same as
subscription plan remaining. For fleet backoff and host dashboards, use:

```go
pu, err := claudia.QueryPlanUsage(ctx, &claudia.PlanUsageArgs{
    Provider: claudia.ProviderClaude, // or ProviderCodex, ProviderGrok, ProviderBedrock
})
// pu.Status: available | unavailable
// pu.Windows: session / weekly with RemainingPercent + ResetsAt when published

all, err := claudia.QueryAllPlanUsage(ctx, nil)
```

| Provider | Behaviour |
| --- | --- |
| Claude | OAuth `GET /api/oauth/usage` → session (5h) + weekly (7d) when signed into Claude.ai |
| Codex | ChatGPT `wham/usage` → windows classified by `limit_window_seconds` |
| Grok | **Unavailable** (no documented SuperGrok remaining API) |
| Bedrock | **Unavailable** (no subscription remaining surface) |

Never invent numbers: missing auth, HTTP errors, or unpublished windows
yield `Status == PlanUsageUnavailable` with an explicit `Reason`. Full
semantics: [docs/plan-usage.md](docs/plan-usage.md). Grok research:
[docs/grok-usage-billing.md](docs/grok-usage-billing.md).

## Task mode: essential patterns

Construct with `NewTask`, then call `Run` to get a channel of
events:

```go
task := claudia.NewTask(claudia.TaskConfig{
    ID:      "unique-id",
    WorkDir: "/abs/path",
    Model:   "sonnet", // or "opus", or "" for default
})
events, err := task.Run(ctx, prompt)
```

The channel closes when the process exits. Drain it until then:

```go
for ev := range events {
    switch ev.Type {
    case claudia.TaskEventInit:
        // ev.SessionID — capture if you want to resume later
    case claudia.TaskEventText:
        // ev.Content — assistant text
    case claudia.TaskEventToolUse:
        // ev.ToolName, ev.ToolInput (JSON string), ev.ToolID
    case claudia.TaskEventResult:
        // ev.Content — final text
        // ev.CostUSD, ev.Usage, ev.DurationMs — accounting
    case claudia.TaskEventError:
        // ev.ErrorMsg — task failed
    }
}
```

**Resuming**: set `TaskConfig.ClaudeID` to the session ID captured
from a prior `TaskEventInit`. claudia passes `--resume <id>` to
`claude`.

**Raw logging**: `Task.SetRawLog(func(line []byte))` gets every NDJSON
line from `claude` before parsing — useful for debugging or custom
processing.

**Cancellation**: `Task.Cancel()` sends SIGINT to the running
process; `Task.Stop()` cancels and marks the task as stopped so
it cannot be re-run.

## Session mode: essential patterns

```go
agent, err := claudia.Start(claudia.Config{
    WorkDir: "/abs/path",
    Model:   "opus",
})
defer agent.Stop()
```

`Start` returns as soon as the `claude` process has been spawned,
which is before the TUI has finished painting its startup UI. You
do **not** need to sleep or poll: the first `Send` blocks
internally until the TUI has gone quiet for 500 ms, which on a
typical standalone session takes about 1.2 s from `Start`. If you
want to observe the ready transition (e.g. to update a spinner),
call `agent.WaitReady(ctx)` explicitly — it returns nil once the
TUI is ready, or an error if detection gave up.

Subscribe to events **before** sending the first message — messages
may arrive quickly. Multiple subscribers are supported; each receives
every event independently:

```go
token := agent.SubscribeEvents(func(ev claudia.Event) {
    // ev.Type: "assistant", "user", "system", "progress", ...
    // ev.Text: concatenated text for assistant turns
    // ev.Usage: token counts (populated on assistant events)
    // ev.Raw:  complete JSONL line
})
defer agent.UnsubscribeEvents(token)

agent.Send("prompt")  // Enter key appended; newlines inside msg are preserved as multi-line input
reply, err := agent.WaitForResponse(ctx)  // blocks until the turn's terminal stop_reason
```

For a one-shot, use the package-level helper:

```go
reply, err := claudia.Run(ctx, "prompt", cfg)
```

It bundles Start + Send + WaitForResponse + Stop.

**Interrupting**: `Interrupt()` sends ESC to the PTY, cancelling the
current turn without killing the process.

**Terminal output**: Session mode captures the raw PTY byte stream to
a log file at
`$XDG_STATE_HOME/claudia/terms/<escaped-workdir>/<sessionID>.term`
(defaulting to `~/.local/state/...` when the XDG var is unset).
Override via `Config.TermLogPath`; set to `"-"` to disable. This file
contains ANSI escapes, cursor moves, and progress bars — it is the
rendered terminal view, not a structured feed. The JSONL transcript
is authoritative for logical content.

**Live terminal streaming**: `SubscribeTerminal()` returns the
buffered history and a live channel of PTY chunks. Always call
`UnsubscribeTerminal(ch)` when done. Subscribers that don't drain
their channel drop data (sends are non-blocking).

**Usage accounting**: `Agent.Usage()` returns cumulative token counts
parsed from the JSONL transcript. The counts accumulate across turns
for the lifetime of the agent. Unlike Task mode (which reports per-prompt
cost in `TaskEventResult`), Session mode totals usage over the whole
session — this is intentional: a persistent session doesn't have clean
per-turn billing boundaries from the API's perspective.

**Verifying the model (no silent fallback)**: the `Model` you pass in
`Config`/`TaskConfig` is the model you *requested*; the model the backend
actually *resolved* is reported back — `Agent.Model()` (Session mode) and
`Task.Model()` / `TaskEvent.Model` on the init event (Task mode). Claude
aliases like `"opus"` resolve to a full id (`"claude-opus-5"`); Bedrock
reports the ModelID passed to ConverseStream; Codex app-server reports
`result.model` on thread start. There is **no** model allowlist in the
public API — correctness is resolution observability + fail-loud errors.

Claude does not fail fast on a bad `--model`: it echoes the string on
init, then fails mid-turn with `message.model` `"<synthetic>"`,
`error: "model_not_found"`, and (in stream-json) a result with
`is_error: true` even when `subtype` is `"success"`. Task mode surfaces
that as `TaskEventError` whose `ErrorMsg` names the model. Session mode
sets `Event.IsError` and `WaitForResponse` returns that text as an
`error` immediately — it never hangs to context timeout and never
treats the synthetic message as a normal reply. The model is fixed at
process launch: pass it on every spawn; it cannot be changed on an
already-running or attached instance except via an in-session `/model`.

**Readiness detection**: The TUI-ready detector polls `tmux capture-pane`
every 50 ms and gives up after 30 s. These values are fixed and not
exposed via `Config`. On macOS the typical ready time is ~680 ms; the
30 s cap exists only as a safety net for pathological cases. If a
consumer consistently hits the cap, file an issue — the values were
chosen empirically and can be revised with evidence.

**Rewinding a session**: `Agent.Rewind(n, cfg)` rolls a session back by
`n` user turns and resumes it, returning a fresh `*Agent` at the rewound
state (the receiver is stopped):

```go
agent2, err := agent.Rewind(2, cfg)  // undo the last two user turns
```

It kills the live `claude` process (which holds the conversation in
memory), truncates the JSONL transcript at the turn boundary, and starts
a new process with `--resume` — which replays only the surviving prefix.
Tool-result entries (recorded with role `user`) are **not** counted as
turns, so a rewind never lands mid-tool-use. The full pre-rewind
transcript is copied to a `.rewind-bak` sidecar, so the rewind is
undoable with `claudia.Unrewind(path)`.

For a transcript-level rewind decoupled from the process lifecycle (e.g.
Task mode, or rewinding a stopped session before the next `Run`), use the
package function directly — stop any live process on the session first:

```go
res, err := claudia.RewindSession(sessionID, workDir, 2)
// res.BackupPath, res.TurnsRemoved, res.BytesRemoved
```

Rewind is conversation-only: it rolls back the transcript, not any
working-tree changes the agent made. Pair it with a git snapshot if you
need code state restored too.

## Registry (optional)

`claudia.Registry` persists agent definitions to a JSON file and
manages their processes. Useful when the host program needs to:

- Auto-start several agents on boot
- Resume agents by name across program restarts
- Rename or reassign agents without losing session history

Construct with `NewRegistry(path)`, then `Register` / `EnsureAgent`
to add definitions and `Launch` / `StartAll` to launch them. If the
host program owns a single short-lived agent, skip the Registry.

## Gotchas

1. **`tmux` must be on `$PATH`; `claude` must be resolvable.** claudia
   shells out to both CLIs; there is no in-process API. `tmux` 3.0+ is
   required for Session mode (`brew install tmux` / `apt install tmux`).
   `claude` is located via `CLAUDE_BIN` (env var, absolute path or
   PATH-resolvable name), then `exec.LookPath`, then known install
   dirs (`~/.local/bin/claude`, `~/.claude/local/claude`,
   `/opt/homebrew/bin/claude`, `/usr/local/bin/claude`). Set
   `CLAUDE_BIN` when running under launchd / systemd / a Windows
   Service whose `$PATH` excludes user-local install dirs. Windows is
   not supported; use WSL. Task mode does not require tmux. Codex
   resolver checks `CODEX_BIN`, then `codex` on `$PATH`, then known
   locations including `/Applications/ChatGPT.app/Contents/Resources/codex`
   (post desktop merger) and the legacy
   `/Applications/Codex.app/Contents/Resources/codex`. Codex Task mode
   also runs a subscription auth preflight before spawn: ChatGPT OAuth
   (`auth_mode=chatgpt` + `tokens.access_token` in `~/.codex/auth.json`,
   or `CLAUDIA_CODEX_AUTH_PATH`) is required; if `OPENAI_API_KEY` is set
   or auth falls through to API-key mode, the spawn fails closed with a
   loud warning so the no-per-token path is verified, not assumed.
   Grok Build CLI resolver checks `GROK_BIN`, then `grok` on `$PATH`,
   then known locations including `~/.grok/bin/grok`.

   Current provider capability matrix:

   | Capability | Claude | Codex | Grok Build CLI |
   |------------|--------|-------|----------------|
   | Task prompts | Supported | Supported via `codex exec --json` | Supported via `grok -p --output-format streaming-json` |
   | Task resume | Supported | Supported via `codex exec resume --json` | Supported via `--resume` |
   | Task usage / cost | Supported | Tokens yes; cost unavailable | Not on streaming-json (no tool_use/cost events); SuperGrok `/usage` panel has no public API ([docs/grok-usage-billing.md](docs/grok-usage-billing.md)) |
   | Persistent Session | Supported | Experimental fail-closed | Supported via ACP (`grok agent stdio`) |
   | Rewind | Supported | Unsupported without public fork/resume proof | Unsupported (no private session-file rewrite) |
   | tmux attach | Supported | Unsupported | Unsupported (ACP is process-local; AttachCommand empty) |
   | Terminal byte log | Supported | Unsupported | Unsupported |
   | Permission mode | Supported | Unsupported — Codex sandbox/approval flags are Codex-native, not a Claude mapping | Unsupported — Task hardcodes `--permission-mode bypassPermissions` |
   | Tool restrictions (`DisallowTools`) | Supported | Unsupported — `codex exec` has no per-tool disallow flag, so `Task.Run` **refuses** the run | Unsupported — `grok` *has* `--deny` and `--disallowed-tools`, but claudia translates `DisallowTools` into neither, so `Task.Run` **refuses** the run |
   | Image inputs | Unsupported (no claudia API on any provider) | Unsupported | Unsupported |
   | Web search | Supported (WebSearch tool, restrictable) | Unsupported — claudia does not bind `--search` | Unsupported |

   This table is generated from the same claims production reads. Query
   it with `claudia.ProviderCapabilityMatrix(provider)`, or gate one
   call with `claudia.CheckCapability(provider, capability)`, which
   returns the `*claudia.CapabilityError` the operation would return.
   Unknown providers and unclaimed capabilities report
   `CapabilityUnsupported` — silence never reads as parity with Claude.

2. **Sub-agents are disabled — on Claude.** Claude Session and Task
   modes always pass
   `--disallowedTools Agent,TeamCreate,TeamDelete,SendMessage,EnterWorktree`.
   The host Go program owns the process lifecycle; nested claudia
   sessions would fight over PTY ownership and transcript tailing.
   Don't try to re-enable these.

   Those are Claude Code tool names, and `BaseDisallowedTools` is
   applied on Claude only — never on Codex, Grok or Bedrock. Rather
   than pretend otherwise, the non-Claude providers report
   `CapabilityToolRestrictions` as unsupported, and a Codex **or Grok**
   task carrying `DisallowTools` is refused outright rather than run
   with the restriction dropped (see the matrix above).

   The two refusals have different causes, and the published reasons
   say which. `codex exec` has no per-tool disallow flag at all. `grok`
   does — `--deny <RULE>` gates invocations and `--disallowed-tools
   <IDS>` strips tools from the toolset — but claudia drives Grok Task
   with a hardcoded `--permission-mode bypassPermissions`, which `grok`
   resolves by appending a catch-all allow rule, and `grok` accepts
   tool names it does not recognise without complaint. An untranslated
   name would therefore be dropped exactly as silently as it is today,
   under a claim that said otherwise. The gap is claudia's, not Grok's,
   and closing it means wiring the translation, the argv builder and
   the claim in one change.

3. **Session resumption is automatic.** `Start` checks whether
   `<SessionID>.jsonl` exists under Claude Code's project directory.
   If it does, claudia passes `--resume`; otherwise `--session-id`.
   Pass a stable `SessionID` to get resumption for free.

4. **Terminal log files are append-only, with no run-boundary markers.**
   Resumed sessions concatenate PTY output across runs — this is a
   deliberate choice. The `.term` file is a raw rendering aid for
   human operators (e.g. via `tmux attach`), not a structured
   transcript. The JSONL file is authoritative for logical content and
   carries its own timestamps. Don't treat the `.term` file as a
   structured single-session record.

   `TermLogPath()` returns `""` if logging is disabled (`Config.TermLogPath
   = "-"`) or if a write error silently halted the log mid-session. Check
   the return value rather than caching the path from `Config`.

5. **`WaitForResponse` replaces the event handler.** It installs its
   own callback (chaining to the previous one) and restores the old
   one on return. Don't stack multiple `WaitForResponse` calls
   concurrently on the same agent.

6. **Both modes strip `CLAUDECODE`.** When a Go program running
   under Claude Code spawns a nested `claude`, claudia removes the
   `CLAUDECODE` env var from the child's environment so it doesn't
   detect itself as a nested session. Applies to both Task and
   Session mode. Don't re-add it.

7. **PTY close races with log writes.** `Stop` serialises termLog
   close with in-flight PTY writes via `termMu`. If you build on top
   of `pushTermOutput` or subscribe to terminal output, respect the
   same mutex discipline.

## tmux substrate

Session mode agents run inside windows on a dedicated claudia tmux
server (socket at `$XDG_STATE_HOME/claudia/tmux.sock`, defaulting
to `~/.local/state/claudia/tmux.sock`). The server starts
automatically on the first `Start` or `Acquire` call — no launchd
or systemd setup is needed.

### Human observability: AttachCommand

Every agent exposes `AttachCommand()` which returns the exact tmux
invocation to attach to its window:

```go
fmt.Println(agent.AttachCommand())
// e.g. tmux -S ~/.local/state/claudia/tmux.sock attach -t @3
```

Run that command from a terminal to watch the live Claude Code TUI.
This is the primary debugging tool when an agent is misbehaving.

### Session-chain tracker (cmd/claudiad)

`cmd/claudiad` is an experimental sidecar (🎯T1.3, not yet fully
shipped) that tracks session chains across `/clear` rollovers.
It is separate from the tmux server and is not required for normal
library operation. `LookupChain` and `ErrDaemonUnavailable` were
removed in the daemon pivot — session-chain lookup will be
filesystem-backed when 🎯T1.3 ships.

## grok subpackage

`github.com/marcelocantos/claudia/grok` is a Grok Realtime voice API
client. It is independent of the rest of claudia — a separate concern
that happens to live in the same module because the original use case
was voice-driving a claudia agent. If you're integrating voice +
Claude Code, wire `grok.Config.OnFunctionCall` to a `claudia.Task`
`Run` invocation and relay results via `InjectAssistantText`.
Otherwise, ignore it.

## Testing

The test suite has two tiers:

**Hermetic tests** run anywhere — no `claude` binary, no Anthropic
credentials, no API cost. They cover event parsing, WaitForResponse
settle semantics, terminal-log path derivation, readiness detection,
and the full tmux control-mode mock machinery. CI runs these on every
push, on both macOS and Linux.

**Live tests** are env-gated and require the matching CLI binary:

| Gate | Provider | Examples |
|------|----------|----------|
| `CLAUDIA_LIVE=1` | Claude | Agent send/receive, pool, `TestTaskRunSmoke` |
| `CLAUDIA_CODEX_LIVE=1` | Codex | `TestCodexTaskRunSmoke` |
| `CLAUDIA_GROK_LIVE=1` | Grok Build CLI | `TestGrokTaskRunSmoke` |
| `CLAUDIA_BEDROCK_LIVE=1` | AWS Bedrock | `TestBedrockTaskLiveSmoke` |

CI does **not** set these — live runs need local auth and may spend
API credit. Bedrock needs work-account AWS credentials and model access
([docs/bedrock-work-account.md](docs/bedrock-work-account.md)).

The canonical pre-release validation command (run locally before
tagging a release):

```sh
CLAUDIA_LIVE=1 go test -race -count=1 ./...
# optional provider smokes when those CLIs/APIs are installed and authed:
# CLAUDIA_CODEX_LIVE=1 CLAUDIA_GROK_LIVE=1 CLAUDIA_BEDROCK_LIVE=1 go test -race -count=1 ./...
```

## Stability

claudia is pre-1.0. `STABILITY.md` in the repo root tracks the public
interaction surface and flags which parts are stable, under review,
or still fluid. Consult it before building consumers that assume long
term API stability.

# claudia

Go library for embedding [Claude Code](https://claude.com/claude-code),
[Grok](https://x.ai/cli), Codex, Bedrock, Ollama, and
[Cursor](https://cursor.com/docs/cli/acp) agents in any program.

claudia wraps each provider's CLI or API in two complementary generation modes
(Task and Session), and offers Judge mode for typed Jev questions. Session transports differ by provider: Claude uses
a tmux PTY plus JSONL tail; Grok uses ACP over stdio or connect-mode
WebSocket; Codex uses `codex app-server` JSON-RPC. Cursor uses ACP over `agent acp`. Bedrock and Ollama
are Task-only HTTP paths.

## Requirements

- Go 1.26+
- macOS or Linux (Windows is not supported; WSL works)
- Per provider, as used:
  - Claude: `claude` CLI (`CLAUDE_BIN` or `$PATH` / known install dirs); tmux 3.0+ for Session mode
  - Grok: `grok` CLI (`GROK_BIN` or `$PATH` / `~/.grok/bin/grok`)
  - Codex: `codex` CLI (`CODEX_BIN` or `$PATH` / known app-bundle paths)
  - Bedrock: AWS SDK default credential chain
  - Ollama: a local daemon (`CLAUDIA_OLLAMA_ENDPOINT`, default `http://127.0.0.1:11434`)
  - Cursor: `cursor-agent` / `agent` CLI (`CURSOR_BIN` or `$PATH` / `~/.local/bin/agent`)

No launchd or systemd setup is needed. Claude Session mode uses tmux for
process lifetime; Grok and Codex Session modes are stdio processes (or
Grok connect-mode `grok agent serve`).

**Grok Build CLI** ships via `ProviderGrok` for both Task mode and
Session mode. Binary discovery checks `GROK_BIN`, then `grok` on `$PATH`,
then known install locations including `~/.grok/bin/grok`. This is the
terminal coding agent from [x.ai/cli](https://x.ai/cli), not the Realtime
voice client in package `claudia/grok`. Session mode uses ACP over
`grok agent stdio` (not tmux).

**Codex Task mode** ships via `ProviderCodex`. The resolver checks
`CODEX_BIN`, then `codex` on `$PATH`, then known install locations
including `/Applications/Codex.app/Contents/Resources/codex`. Codex
Session mode uses `codex app-server` JSON-RPC (not tmux).

**Cursor Agent CLI** ships via `ProviderCursor` for Session and Task.
Binary discovery checks `CURSOR_BIN`, then `cursor-agent` on `$PATH`,
then `~/.local/bin/agent`, then a PATH `agent` that is not Grok's
`~/.grok/bin/agent`. Session mode uses ACP over `agent acp` (not tmux).
Task mode uses `agent --print --output-format stream-json`. Auth is
`agent login` or `CURSOR_API_KEY`. See
[docs/cursor-acp-session.md](docs/cursor-acp-session.md).

**AWS Bedrock Task mode** ships via `ProviderBedrock` — Anthropic Claude
models through Bedrock **ConverseStream** (API path; no local `claude`
CLI). Credentials use the AWS SDK default chain (`AWS_PROFILE`, env keys,
SSO, roles). Region: `CLAUDIA_BEDROCK_REGION` or `AWS_REGION` /
`AWS_DEFAULT_REGION`. Model: `TaskConfig.Model` or
`CLAUDIA_BEDROCK_MODEL_ID`. Session/resume/tools are **not** claimed in
v1. Setup: [docs/bedrock-work-account.md](docs/bedrock-work-account.md).
Design: [docs/bedrock-provider.md](docs/bedrock-provider.md).

## Modes

### Task mode — one-shot prompts

Spawns `claude` with `--output-format stream-json`, streams structured
events, and exits. Use it for code generation, analysis, or
transformation tasks with a clear end, and for anything where you want
a single prompt → single result with cost and token accounting.

```go
task := claudia.NewTask(claudia.TaskConfig{
    ID:      "gen-1",
    WorkDir: "/path/to/repo",
    Model:   "sonnet",
})

events, err := task.Run(ctx, "Summarise the public API of this module.")
if err != nil {
    log.Fatal(err)
}
for ev := range events {
    switch ev.Type {
    case claudia.TaskEventText:
        fmt.Print(ev.Content)
    case claudia.TaskEventResult:
        fmt.Printf("\n(%.2fs, $%.4f)\n", ev.DurationMs/1000, ev.CostUSD)
    }
}
```

Resume a prior task session by setting `TaskConfig.ClaudeID` to the
session ID captured from a previous `TaskEventInit`.

Codex Task mode is available by selecting `ProviderCodex`. It runs
`codex exec --json`, captures the Codex thread id as the task session
id, and can resume with the same `TaskConfig.ClaudeID` field:

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider:       claudia.ProviderCodex,
    ID:             "codex-summary",
    WorkDir:        "/path/to/repo",
    Model:          "gpt-6-sol",
    SandboxMode:    "workspace-write",
    ApprovalPolicy: "on-request",
})
```

Codex live tests are opt-in because they use local Codex credentials
and may contact OpenAI. Run them with `CLAUDIA_CODEX_LIVE=1`.

Grok Task mode is available by selecting `ProviderGrok`. It runs
`grok -p … --output-format streaming-json`, captures the session id
from the terminal `end` event, and can resume with the same
`TaskConfig.ClaudeID` field:

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider: claudia.ProviderGrok,
    ID:       "grok-summary",
    WorkDir:  "/path/to/repo",
    Model:    "grok-4",
})
```

Grok live tests are opt-in (`CLAUDIA_GROK_LIVE=1`). Auth is whatever the
installed `grok` CLI already uses (`grok login` or `XAI_API_KEY`).
SuperGrok weekly usage / Extra Credits and console prepaid balance are
**not** Task streams — see
[docs/grok-usage-billing.md](docs/grok-usage-billing.md).

**Plan remaining (all providers):** `QueryPlanUsage` /
`QueryAllPlanUsage` expose subscription session + weekly % remaining and
rollover times when a backend publishes them. Grok and Cursor unofficial
billing surfaces are always fetched; a break is unavailable-with-reason
(never invented numbers). Bedrock has no subscription window. See
[docs/plan-usage.md](docs/plan-usage.md).

Bedrock Task mode is available by selecting `ProviderBedrock`. It calls
AWS Bedrock ConverseStream and maps text deltas to `TaskEventText`:

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider: claudia.ProviderBedrock,
    ID:       "bedrock-summary",
    Model:    "anthropic.claude-3-5-sonnet-20241022-v2:0", // or inference profile id
})
```

Bedrock live tests are opt-in (`CLAUDIA_BEDROCK_LIVE=1`) and need work-account
credentials plus model access (see docs/bedrock-work-account.md).

**Ollama Task mode** (`ProviderOllama`) calls a local `/api/generate`
endpoint. Override with `CLAUDIA_OLLAMA_ENDPOINT` / `CLAUDIA_OLLAMA_MODEL`.
Session, resume, and tools fail closed. Cost is latency, not tokens.

### Session mode — persistent conversations

Claude Session mode spawns `claude` inside a tmux window on a dedicated
claudia tmux server and keeps it alive. Grok Session uses ACP (`grok
agent stdio` or connect-mode serve); Codex Session uses `codex
app-server` JSON-RPC. There is no tmux window for Grok or Codex.
Use Session mode for multi-turn conversations, interactive agents that
respond to external events, or programs that need to observe the session
as it happens. Claude's tmux substrate provides crash-survival (the
agent outlives the consumer) and human-attachable observability — you
can inspect a live Claude agent with:

```sh
tmux -S ~/.local/state/claudia/tmux.sock attach -t <window>
```

(`AttachCommand()` on the agent returns the exact invocation.)

```go
agent, err := claudia.Start(claudia.Config{
    WorkDir: "/path/to/repo",
    Model:   "opus",
})
if err != nil {
    log.Fatal(err)
}
defer agent.Stop()

tok := agent.SubscribeEvents(func(ev claudia.Event) {
    if ev.Type == "assistant" {
        fmt.Println(ev.Text)
    }
})
defer agent.UnsubscribeEvents(tok)

if err := agent.Send("What does this repo do?"); err != nil {
    log.Fatal(err)
}
reply, err := agent.WaitForResponse(ctx)
```

Same-provider model change is `agent.SetModel("sonnet")`. Switching
providers on a live Session is
`agent.Migrate(&claudia.MigrateArgs{Provider: claudia.ProviderGrok})`:
a new native destination session, an inert distilled seed, and the
same `*Agent` handle and event subscriptions. Claudia does not
auto-migrate. See [agents-guide.md](agents-guide.md).

`Config.Goal` (also `AgentDef.Goal`) is a host-owned Session
objective. Empty keeps one-shot `Send`. When set, the Agent issues a
continuation `Send` after each terminal assistant turn until `Stop`,
`Interrupt`, `Agent.CloseGoal`, `Config.GoalCompleteCheck` returning
true, or an assistant line `GOAL_STATUS: complete` /
`GOAL_STATUS: blocked`. The string is not forwarded to any provider
`/goal` command, so the same Goal can ride a later `Start` on a
different Provider.

MCP is Claudia's job. Callers do not write `~/.claude.json`,
`~/.grok/config.toml`, or `~/.codex/config.toml`.

```go
inv, err := claudia.LoadMCP(nil) // Claude + Grok + Codex maps
cfg.MCPServers = inv.ForProvider(cfg.Provider)
inv.Servers = append(inv.Servers, claudia.MCPServer{
    Name: "jevonsmcp", Type: "http", URL: "http://127.0.0.1:13705/mcp",
})
p, err := claudia.NewMCPProxy(&claudia.MCPProxyArgs{
    Prefix: "/upstream", PublicBase: "http://127.0.0.1:13705",
    Servers: inv.Servers,
})
mux.Handle("/upstream/", p)
```

`LoadMCP` tags each server with origin `Providers` (Codex-only
computer-use stays off Claude). `NewMCPProxy` is an `http.Handler`.
On `claudia broker serve` the daemon mounts it, hosts stdio MCP, and
rewrites grant `MCPServers` to loopback URLs (🎯T2.16). Direct mode
still attaches the caller's list. Owner-present OAuth is
`AuthorizeMCP`; the daemon persists refresh tokens.
`Config.MCPServers` is the only Session attach path (inline/temp
`--mcp-config`, ACP `mcpServers`, or process-private Codex home) —
Claudia never writes user or project MCP files.
`Config.MCPExclusive` (default false) keeps
user-scope maps additive where the backend still loads them; set true
for hermetic Claude/Grok/Codex isolation. Cursor exclusive is ACP
`mcpServers` only (no project mcp.json / HOME rewrite; real-home auth
or `CURSOR_API_KEY`).

**Picking a model.** `Resolve` chooses a catalog `(Provider, Model)` from
predicates. The catalog is a set, not a ranking: available-tokens is a
veto, then destination bands and plan pressure rank survivors. On the
purpose-quality path, `PreferProvider` wins among token-eligible rows.
Set `Background` for low-priority work: it excludes orange/red plans,
including a preferred provider, and returns no pick if none remain.
Unpublished usage remains eligible unless `RequireUsage` is set. With `Purpose` set
(`coding`, `analysis`, `agent`, `browse`, `general`) it also returns
`Effort`: quality is a floor on that job, and generation/effort are
outputs. The daemon refreshes published scores daily into
`StateDir/model-intel` (`claudia models intel …`; needs
`CLAUDIA_AA_API_KEY`). `skill` is a wire alias for `purpose`. A purpose
with no catalog-overlapping observations is interpreted as `general`
(`purpose_fallback_from` on the pick), even when the general series is
empty. A series that exists but misses the floor or is token-exhausted
fails closed. Empty `Purpose` keeps the catalog-shelf path. See
[docs/model-intel.md](docs/model-intel.md).

The one-shot helper `claudia.Run(ctx, prompt, cfg)` bundles `Start` +
`Send` + `WaitForResponse` + `Stop` for session mode if you want a
single call.

Fail-closed resume: set `Config.RequireResume` when `SessionID` names an
existing conversation — a failed load is then a hard error instead of a
silent fall-through to a fresh session, so a conversation can never be
orphaned by a resume failure. `Registry`-managed agents get this once
`AgentDef.Materialized` records a real conversation (Claude session
JSONL present, or `Registry.MarkMaterialized` after the first completed
turn) — not merely because `Start` succeeded.

Resuming works automatically: if `Config.SessionID` is set and a JSONL
transcript already exists for it, claudia passes `--resume`; otherwise
it passes `--session-id` to create a fresh session with that ID.

Grok Session mode is available with `Config{Provider: claudia.ProviderGrok}`:

```go
agent, err := claudia.Start(claudia.Config{
    Provider: claudia.ProviderGrok,
    WorkDir:  "/path/to/repo",
    Model:    "grok-4", // optional
})
```

It runs `grok agent --always-approve stdio`, speaks ACP JSON-RPC, and
supports `Send` / `WaitForResponse` / `Interrupt` / `Stop`. There is no
tmux attach window. Rewind remains unsupported (`CapabilityUnsupported`).

`Config{Provider: claudia.ProviderCodex}` starts `codex app-server` over
stdio (thread/turn JSON-RPC). There is no tmux attach window. Rewind
remains unsupported. claudia does not scrape private Codex session files.

Provider gaps are published, not discovered at runtime. Every capability
claudia reports on carries an explicit `supported` / `unsupported` /
`experimental` status per provider:

```go
status := claudia.ProviderCapabilityStatus(
    claudia.ProviderCodex, claudia.CapabilityRewind) // "unsupported"

if err := claudia.CheckCapability(
    claudia.ProviderCodex, claudia.CapabilityToolRestrictions); err != nil {
    // *claudia.CapabilityError, with the documented rationale.
}
```

`ProviderCapabilityMatrix(provider)` returns the whole table. Unknown
providers and unclaimed capabilities report `CapabilityUnsupported`:
silence never reads as parity with Claude. See
[STABILITY.md](STABILITY.md) for the current six-provider matrix —
including that a Codex task carrying `DisallowTools` is refused, because
`codex exec` has no per-tool disallow flag to honour it with.

Claude Session also captures PTY output to
`$XDG_STATE_HOME/claudia/terms/<escaped-workdir>/<sessionID>.term`
(defaulting to `~/.local/state/...`) so you have a faithful record of
the rendered terminal view alongside the structured JSONL transcript.
Override via `Config.TermLogPath`; set to `"-"` to disable. Grok and
Codex Session have no PTY log (`CapabilityTerminalLog` unsupported).

## Daemon (optional)

Install the daemon with Homebrew, then start it as a user service:

```bash
brew install marcelocantos/tap/claudia
# supervisor-hosted machines (this host):
make supervisor-install
# other machines:
brew services start claudia
```

`claudia broker serve` (or `make supervisor-install` / `brew services
start claudia` / `claudia broker install` for a launchd user agent) runs a host-wide daemon that
owns every consumer's agent processes: Sessions become named grants that survive the consumer's
restart, Tasks run on the daemon, plan usage is fetched once per host,
and after a reboot the daemon resumes the seats it held and tells them
so. The library API is unchanged; with no socket (or
`CLAUDIA_NO_BROKER=1`) everything runs in-process as before. See the
[agents guide](agents-guide.md#daemon-claudia-broker-optional-host-wide).

## Registry

For long-lived programs that manage several persistent agents
(auto-start on boot, resume by name, track definitions across program
restarts), claudia ships a `Registry` type that persists agent
definitions to a JSON file and manages their processes.

```go
reg, _ := claudia.NewRegistry("/var/lib/myapp/agents.json")
reg.EnsureAgent("reviewer", "/path/to/repo", "sonnet", true)
reg.StartAll() // starts every agent marked AutoStart
defer reg.StopAll()
```

If the host program owns a single short-lived agent, skip the Registry
and call `Start` directly.

## tmux substrate

Claude Session mode agents run inside a dedicated claudia tmux server
(socket at `$XDG_STATE_HOME/claudia/tmux.sock`, defaulting to
`~/.local/state/claudia/tmux.sock`). Each Claude agent occupies one
tmux window. The server starts automatically on the first Claude
`Start` or `Acquire` call and persists until the machine reboots — no
launchd or systemd configuration is needed.

Because Claude agents live in tmux, they survive consumer process death.
A new consumer process can reconnect to an existing window (via
`Acquire` with a matching pool key) or observe its transcript via the
JSONL file that Claude Code writes to `~/.claude/projects/`. Grok
connect-mode (`Config.GrokConnect`) is the analogous outlive-the-consumer
path for Grok; Codex Session is process-local stdio.

Session-id chains across resume/rotate are filesystem-backed via
`RegisterChain` / `LookupChain`. The tmux server is not a `claudiad`
sidecar — Claude Session lifetime is tmux. The optional host-wide
process is `claudia broker serve` (see [Daemon](#daemon-optional)).

## grok subpackage

`github.com/marcelocantos/claudia/grok` is a standalone client for
xAI's Grok Realtime voice API. It bridges full-duplex voice I/O with
function calling for agent delegation — you can wire Grok's tool calls
to a claudia Task to produce a voice-driven coding agent. It's
independent of the rest of the package; use it if you want, ignore it
if you don't.

## Installation

```bash
go get github.com/marcelocantos/claudia@latest
```

See [Requirements](#requirements) above for runtime dependencies.

For an agent-assisted integration, you can paste this prompt into your coding tool:

```text
Add github.com/marcelocantos/claudia to this Go project. Read its agents-guide.md first, choose Task, Session, or Judge for my use case, and verify the integration with a test that exercises the chosen path.
```

Judge mode asks TypeSafe's Jev a set of typed yes/no, choice, or scale
questions and returns full answer distributions. See
[Judge mode in the agent guide](agents-guide.md#judge-mode-typed-questions-jev).

## For agents

If you use an agentic coding tool, include
[`agents-guide.md`](agents-guide.md) in your project context — it
covers the API surface, common patterns, and gotchas in a form
designed for LLM consumption. The installed CLI also prints it via
`claudia --help-agent`.

The public API surface and its stability are tracked in
[`STABILITY.md`](STABILITY.md). claudia is pre-1.0; breaking changes
are possible until 1.0 locks in a backwards-compatibility contract.

## Licence

Apache 2.0 — see [LICENSE](LICENSE). Notices and licence terms for
linked dependencies are in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).

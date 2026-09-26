# claudia — agents guide

`github.com/marcelocantos/claudia` is a Go library for embedding
Claude, Grok, Codex, Bedrock, Ollama, and Cursor agents in your program,
and for asking TypeSafe's Jev typed questions (Judge mode).

```
go get github.com/marcelocantos/claudia
```

```go
import "github.com/marcelocantos/claudia"
```

If you're helping a user integrate it, read this whole document first —
the design is small but has non-obvious constraints.

## Pick the right mode

claudia offers two generation modes. They are not interchangeable; choose
based on the shape of the work. Judge is a separate typed-question mode.

|                     | Task mode                            | Session mode                            |
|---------------------|--------------------------------------|-----------------------------------------|
| Type                | `claudia.Task`                       | `claudia.Agent`                         |
| Process model       | New process (or HTTP call) per prompt | Persistent process: tmux PTY (Claude); ACP stdio/WebSocket (Grok); `codex app-server` JSON-RPC (Codex) |
| Output              | Structured events (JSONL / stream-json / HTTP chunks) | Events via JSONL tail (Claude) or in-process RPC (Grok ACP, Codex app-server). Claude also captures a raw PTY log. |
| Use case            | One-shot generation / analysis       | Multi-turn conversations                |
| Cost accounting     | Yes when the provider reports it (`TaskEvent`) | Cumulative via `Agent.Usage()` when the transcript carries tokens |
| Resume across runs  | Via `TaskConfig.ClaudeID` (the name is reused for every provider's session id) | Via `Config.SessionID` |

**Default to Task mode.** It's simpler, gives you structured events,
and exposes cost and token accounting. Only use Session mode if the
user explicitly needs persistent state or wants to observe the
transcript live.

A third mode, **Judge** (`claudia.Judge`), is for work that is not
generation at all: a typed judgment (yes/no, one of a set, a position
on a scale) with a probability, from a System One model (TypeSafe's
Jev) in under a second. See [Judge mode](#judge-mode-typed-questions-jev).

### Codex provider (Task mode)

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider:       claudia.ProviderCodex,
    WorkDir:        "/abs/path",
    Model:          "gpt-6-sol",
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

Codex persistent Session mode uses `codex app-server` over stdio
(thread/start, turn/start, thread/resume). `SessionID` is the Codex
thread id (`thr_…`). `RequireResume` fail-closes if `thread/resume`
fails. Rewind, tmux attach, and terminal logs stay unsupported; do
not implement them by editing private Codex storage or driving the
Codex TUI.

#### Codex Session sandbox and `.git`

`Config.SandboxMode` is the app-server sandbox; empty means
`read-only`. `workspace-write` makes `WorkDir` writable and keeps its
`.git` read-only, so by default a seat can edit its repository and
cannot commit to it: `git commit` and `git worktree add` fail with
`Operation not permitted`. `Start` logs a warning naming the directory
when that applies.

```go
agent, err := claudia.Start(claudia.Config{
    Provider:        claudia.ProviderCodex,
    WorkDir:         "/abs/path/to/repo",
    SandboxMode:     "workspace-write",
    SandboxGitWrite: true, // this seat's mission is to commit
})
```

`SandboxGitWrite` grants the seat its repository's git directory (the
shared one under the main checkout, for a linked worktree). `Start`
fails, naming the directory, if Codex reports a sandbox without the
grant, and refuses the field on a read-only seat. It is persisted on
`AgentDef` (`sandbox_git_write`), so a relaunch keeps it.

**It is off by default because it is a sandbox escape, not a
convenience.** Codex protects `.git` for a reason: `.git/hooks/*` and
`.git/config` (`core.hooksPath`, `core.fsmonitor`, aliases, filters) are
code that git runs *outside* the sandbox. A seat with a writable `.git`
can plant a hook, and it executes unsandboxed, as the operator, the next
time anyone — the operator, another agent, an editor's git integration —
runs git in that repository. Set `SandboxGitWrite` only for a seat that
must commit, in a repository whose operator accepts that the seat's
reach is then the operator's own. A seat that only needs to edit files
should leave it unset and let its spawner commit.

The grant covers `.git` only. A worktree added *outside* `WorkDir`
(`git worktree add ../tree`) also needs its destination in
`SandboxWritableRoots`.

Task mode is the same sandbox with a quieter failure: under
`SandboxMode: "workspace-write"`, `codex exec` refuses the commit and
still exits 0. `TaskConfig.SandboxGitWrite` is the same opt-in, with the
same trade-off, and `Run` logs the same warning when it is unset.

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider:        claudia.ProviderCodex,
    WorkDir:         "/abs/path/to/repo",
    SandboxMode:     "workspace-write", // must be spelled out for the grant
    SandboxGitWrite: true,
    ApprovalPolicy:  "never",
})
```

`Run` refuses `SandboxGitWrite` unless `SandboxMode` is
`workspace-write` (or `danger-full-access`, where there is nothing to
grant): an empty `SandboxMode` leaves the mode to the user's
`config.toml`, which claudia does not read, so the grant could land on a
read-only run and be dropped in silence.

In both modes the grant travels as a
`-c sandbox_workspace_write.writable_roots=[…]` override. A `-c` value
replaces the list in `CODEX_HOME/config.toml` rather than adding to it.
A Session repeats its own `SandboxWritableRoots` in the override; roots
the *user* wrote into `~/.codex/config.toml` by hand are not read, and do
not apply to a run that sets `SandboxGitWrite`.

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
as the Grok session id with `--resume`. Plan-usage fetches rotate an
expired login token themselves — a billing 401 runs one headless grok
turn, which rewrites `auth.json`, then retries (🎯T74; see
`docs/plan-usage.md`) — so `grok login` is only needed when the refresh
token itself is gone.

Headless `streaming-json` maps `text` → `TaskEventText`, terminal
`end.sessionId` → `TaskEventInit` then `TaskEventResult`, and
`error` → `TaskEventError`. Thought deltas are ignored. Tool-use
and cost/usage are not present on this public stream — do not expect
Claude-parity accounting or tool events. SuperGrok weekly remaining
is still not a Task stream: `QueryPlanUsage(ProviderGrok)` always
fetches the undocumented billing endpoint and fails loud (unavailable
+ reason) when it breaks — never a fabricated percent.
`grok -p "/usage"` is only a model prompt, not the TUI slash command.
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

`Config.Goal` (also `AgentDef.Goal`) is a host-owned Session
objective. Empty keeps one-shot `Send`. When set, the Agent issues a
continuation `Send` after each terminal assistant turn until `Stop`,
`Interrupt`, `Agent.CloseGoal()`, `Config.GoalCompleteCheck` returning
true, or an assistant line `GOAL_STATUS: complete` /
`GOAL_STATUS: blocked`. It also stops on its own after three
consecutive turns that end without a tool call: the Goal is closed and
a `Type=system`, `ProgressType=goal_stalled` Event says so on the same
stream. Claudia does nothing else; the host decides what a seat that
answers without working needs. The string is not forwarded to any provider
`/goal` command, so the same Goal can ride a later `Start` on a
different Provider. `SetGoalCompleteCheck` installs the hook after
`Start` / `Launch` when the registry path cannot carry a function on
`AgentDef`.

### Cursor provider (Session + Task)

Cursor Session mode uses ACP over `agent acp`:

```go
agent, err := claudia.Start(claudia.Config{
    Provider: claudia.ProviderCursor,
    WorkDir:  "/abs/path",
    Model:    "grok-4.6", // optional
})
```

Task mode uses `agent --print --output-format stream-json`:

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider: claudia.ProviderCursor,
    WorkDir:  "/abs/path",
})
ch, err := task.Run(ctx, "Reply with exactly: pong")
```

Binary discovery: `CURSOR_BIN`, then `cursor-agent` on `$PATH`, then
`~/.local/bin/agent`, then a PATH `agent` that is not Grok's
`~/.grok/bin/agent`. Auth is `agent login` or `CURSOR_API_KEY`.
`Start` sends `authenticate` with `methodId: "cursor_login"`. Permission
prompts auto-select `allow-always` (hyphenated Cursor option ids).
Blocking `cursor/ask_question` is skipped; `cursor/create_plan` is
accepted so unattended turns do not stall.

Rewind, tmux attach, and terminal logs stay unsupported.

Plan remaining always reads the undocumented dashboard
`GetCurrentPeriodUsage` RPC — same honesty rule as Grok: unavailable
with a reason, never a fabricated percent. A break is a parser fix,
not a gate.

MCP is Claudia's job (🎯T40). Callers name servers and transports;
they do not write `~/.claude.json`, `~/.grok/config.toml`,
`~/.codex/config.toml`, or `~/.cursor/mcp.json`.

```go
inv, err := claudia.LoadMCP(nil) // Claude user-scope map
inv.Servers = append(inv.Servers, claudia.MCPServer{
    Name: "jevonsmcp", Type: "http", URL: "http://127.0.0.1:13705/mcp",
    // Headers / BearerTokenEnv ride with the server when the
    // endpoint needs a static token. OAuth stays in each
    // provider's own vault — Claudia does not copy it.
})
cfg.MCPServers = inv.Servers
```

`Config.MCPExclusive` (default false) is the isolate switch. False
keeps each CLI's user-scope MCP map (additive) where the backend
still loads it. True is hermetic via process-private materialisation:
Claude `--strict-mcp-config`, Grok durable per-session `GROK_HOME` under
`$XDG_STATE_HOME/claudia/grok-homes` (auth copied,
compat MCP discovery off), Codex `CODEX_HOME` persisted under
`$XDG_STATE_HOME/claudia/codex-homes/<sessionID>` containing only
`Config.MCPServers`. Cursor has no strict flag and Claudia does
**not** rewrite project `.cursor/mcp.json` or `HOME` (Keychain);
exclusive Session MCP is ACP `mcpServers` only, so user-scope
`~/.cursor/mcp.json` may still attach. Auth uses the real login or
`CURSOR_API_KEY` / `--api-key`. Jevons wants exclusive; other hosts
can leave the default.

Exclusive Grok homes survive `Stop` and are indexed by the session identity
returned by the provider. A Grok registry row loaded from disk requires resume,
even when its `Materialized` flag is false. Missing homes or refused loads fail
explicitly instead of creating replacement conversations. This does not recover
old temporary homes that were already deleted, or seats saved before their first
successful launch. Existing unmanaged connect endpoints without a durable-home
mapping are refused; same-process adoption is not covered by the restart test.

Restart verification currently has a consumer-level limit: Claudia's real
stdio and serve retained-context tests pass, but Jevons' strict J14 reply check
also requires no extra commentary. A final local consumer run retained the
original message and produced the correct answer after unsolicited tool-search
commentary, so that run remains failed. Storage persistence alone does not
certify the complete Jevons restart interaction (Claudia T57 / Jevons T627.1).

`LoadMCP` reads **each provider's** config (Claude JSON, Grok TOML,
Codex TOML, Cursor `mcp.json`) and tags `MCPServer.Providers`. A Codex-only
computer-use server stays off Claude. `inv.ForProvider(cfg.Provider)`
is the list to attach to a Session. Caller-appended servers with
empty Providers (jevonsmcp) are valid for every backend. `LoadMCP(nil)`
uses the user-scope defaults; any path override means *only*
those paths are read.
`Config.MCPServers` is the only Session attach path: Claude gets an
inline or temp `--mcp-config`, Grok/Cursor get ACP `mcpServers`, and
Codex gets a process-private `CODEX_HOME`. Claudia never writes
`~/.claude.json`, `~/.grok`, `~/.codex`, `~/.cursor`, project
`.cursor/mcp.json`, or workdir `mcp.claudia.json`. Isolates pass
fixture paths on `LoadMCPArgs` so they never read the daily files.
Bedrock and Ollama have no Session MCP surface.

HTTP MCP OAuth (🎯T42): `ProbeMCP` classifies a URL as `open`,
`static`, or `oauth` from one unauthenticated initialize.
`AuthorizeMCP` is owner-present PKCE (browser + local redirect);
tokens are returned to the caller. `RefreshMCPToken` exchanges a
stored refresh_token without a browser.

HTTP MCP proxy (🎯T43) remains an `http.Handler` (`NewMCPProxy`) for
hosts that still mount it themselves. `MCPHost` owns MCP connections
for many seats: it listens on loopback, proxies HTTP remotes, keeps one
stdio process per server recipe, persists OAuth tokens under its state
directory, and `Attach` rewrites servers to
`http://<addr>/upstream/<name>`. A different recipe under an
already-hosted name is left unrewritten so an isolate cannot steal the
daily backend.

```go
host, err := claudia.NewMCPHost(&claudia.MCPHostArgs{StateDir: dir})
reg.SetMCPHost(host) // seats reg starts in-process attach through host
// or, without a Registry: cfg.MCPServers = host.Attach(cfg.MCPServers)
```

`MCPHostArgs.ConsumerOwned` names servers to leave on the caller's URL;
`SeedStateDirs` seeds empty token/upstream stores from a previous
owner. Attaching is opt-in: a Registry without a host, and `Start`,
pass servers through unchanged. On the daemon path (🎯T2.16)
`claudia broker serve` runs one host for every seat it holds, keeps
`jevonsmcp*` on the caller's URL (`daemon.Options.MCPConsumerOwnedPrefixes`),
and seeds from `~/.jevons`.

Pass `SessionID` to attempt `session/load`. A materialized resume
(`RequireResume`) never mints a replacement session: load failure is an
error, including when `MCPConfig` is set. `MCPConfig` is converted to
ACP `mcpServers` and sent on both new and load; it does not skip load.
Without `RequireResume`, a failed load may fall through to `session/new`.
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

### Ollama provider (Task mode only)

```go
task := claudia.NewTask(claudia.TaskConfig{
    Provider: claudia.ProviderOllama,
    ID:       "ollama-1",
    Model:    "llama3.2", // or CLAUDIA_OLLAMA_MODEL
})
```

Ollama is a local HTTP path (`/api/generate` at
`CLAUDIA_OLLAMA_ENDPOINT`, default `http://127.0.0.1:11434`), not a
coding-agent CLI. Cost is latency, not tokens — `CapabilityCost` is
unsupported rather than a spend of zero. **Not claimed:** Session,
resume, rewind, tools, permissions, extra argv. `Start(ProviderOllama)`
fails closed with `CapabilityError`. Live tests:
`CLAUDIA_OLLAMA_LIVE=1` (needs `CLAUDIA_OLLAMA_MODEL`).

## Plan usage (subscription remaining + rollover)

Per-run token `Usage` / `CostUSD` on Task events is **not** the same as
subscription plan remaining. For fleet backoff and host dashboards, use:

```go
pu, err := claudia.QueryPlanUsage(ctx, &claudia.PlanUsageArgs{
    Provider: claudia.ProviderClaude, // or ProviderCodex, ProviderGrok, ProviderBedrock, ProviderCursor
})
// pu.Status: available | unavailable
// pu.Windows: session / weekly with RemainingPercent + ResetsAt when published

all, err := claudia.QueryAllPlanUsage(ctx, nil)
```

Shared refresh (TTL cache under the user cache dir, exclusive lease,
heartbeat steal if the holder goes quiet, write-then-release, recheck
under the lock before a second fetch, waiters poll):

```go
all, err := claudia.LoadPlanUsage(ctx, nil) // CLAUDIA_PLAN_CACHE overrides the dir
```

Bands (ok / ahead / hot / under / locked / exhausted / unpublished) are
classified next to this API — do not re-derive them in the client:

```go
v := claudia.ClassifyPlan(pu, time.Now(), nil)
ok := claudia.HasAvailableTokens(pu, time.Now(), nil)
```

**Picking a model.** Pass predicates, not a model id. The catalog is a
set of spawnable rows, not a ranking. Available-tokens is automatic
(known-exhausted / weekly-hot / session-low are skipped; unpublished is
not a veto). On the catalog path, destination bands and plan pressure
rank survivors. On the purpose-quality path, `PreferProvider` wins among
token-eligible rows. Set `Background` for work that must not use a
provider spending ahead of pace (orange/red), even if preferred; if no
eligible provider remains, Resolve fails without spawning. Unpublished
usage remains eligible unless `RequireUsage` is also set. Resolve does
not spawn.

```go
pick, err := claudia.Resolve(ctx, claudia.ModelPredicates{
    Mode:           claudia.CapabilityTask, // or CapabilitySession
    Quality:        claudia.ModelQualityStandard, // hard filter; empty means standard
    PreferPlan:     true,
    PreferProvider: claudia.ProviderGrok, // optional host preference
    Background:     true, // low-priority work avoids orange/red plans
})
task := claudia.NewTask(claudia.TaskConfig{Provider: pick.Provider, Model: pick.Model, WorkDir: dir})
```

**Purpose-quality (🎯T71).** Set `Purpose` (`coding`, `analysis`, `agent`, `browse`, `general`) to pick from the daily intel series. `Skill` is a client/wire alias for the same field (`skill=analysis`). Quality is then a floor on that purpose, not a generation shelf. Generation and effort come back on the pick; the host does not specify them unless pinning. A purpose with no catalog-overlapping observations is interpreted as `general` — even when the general series is empty — and the pick records `purpose_fallback_from` (hosts can ask for `analysis` without a host-side fallback). A series that exists but misses the floor or is token-exhausted still fails closed. Catalog rows without an AA score stay eligible when they match the quality shelf — token slack can pick Cursor even if composer is missing from the series. Ranking is plan slack, then research `$` per task. Empty `Purpose` keeps the catalog-shelf path above.

```go
pick, err := claudia.Resolve(ctx, claudia.ModelPredicates{
    Mode:    claudia.CapabilityTask,
    Purpose: claudia.ModelPurposeCoding,
    Quality: claudia.ModelQualityStandard,
})
// pick.Model and pick.Effort are outputs
```

Refresh the series with `claudia models intel refresh` (needs `CLAUDIA_AA_API_KEY`). The daemon repeats that at most daily into `StateDir/model-intel`. History and drift are first-class: a later fetch appends; a source-revision change is a board event, not a model move. See [docs/model-intel.md](docs/model-intel.md).

| Provider | Behaviour |
| --- | --- |
| Claude | OAuth `GET /api/oauth/usage` → session (5h) + weekly (7d) when signed into Claude.ai |
| Codex | ChatGPT `wham/usage` → windows classified by `limit_window_seconds` |
| Grok | SuperGrok weekly pool via undocumented `cli-chat-proxy` billing (always fetched; break → unavailable + reason) |
| Cursor | Billing-cycle remaining via undocumented dashboard `GetCurrentPeriodUsage` (always fetched; break → unavailable + reason) |
| Bedrock | **Unavailable** (no subscription remaining surface) |
| Ollama | **Unavailable** (local inference; no subscription remaining surface) |

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

## Judge mode: typed questions (Jev)

`claudia.Judge` asks TypeSafe's System One model typed questions about a
state and returns every answer with its probability distribution
(🎯T127). One request answers many questions over the same state, in
parallel, in about 0.7–0.9 s. API contract:
<https://docs.typesafe.ai/api.md>.

```go
j := claudia.NewJudge(claudia.JudgeConfig{}) // jev-latest, key from env or ~/.typesafe/env
res, err := j.Ask(ctx, claudia.JudgeRequest{
    State: synopsis, // a string, or any JSON-marshalable value
    Questions: map[string]claudia.JudgeQuestion{
        "verdict": {Type: claudia.JudgeChoice, Instructions: "Would a reader be materially misled?",
            Options: map[string]any{"material": "…", "minor": "…", "sound": "…"}},
        "funnel": {Type: claudia.JudgeNoul, Instructions: "Does the argument lead into the speaker's own product?"},
        "rigour": {Type: claudia.JudgeScore, Instructions: "How well sourced is it?",
            Levels: []any{"Unsourced", "Some sources", "Thoroughly sourced"}},
    },
})
// res.Answers["verdict"].Probabilities["material"] — threshold on this
// res.Answers["funnel"].Noul                       — P(yes)
// res.Model  — the release that answered ("jev-1.13.0"); store it with the answer
// res.Usage  — input/output tokens (the API reports no cache fields)
```

- **Threshold on `Probabilities`, not `Choice`.** On a held-out run of 191
  synopses (jev-1.13.0) Jev's top pick was "material" 150 times, while
  `P(material)` ranked material against the rest at AUC 0.75. The pick
  carries nothing; the probability carries a weak signal.
- **Validate the threshold on held-out data.** That same cutoff scored AUC
  0.94 on the 30 cases it was tuned on, and caught 13 of 19 on the 191 it
  had not seen — 54 flagged for a 10% base rate. Tuning-sample numbers do
  not survive. How well Jev does is a property of your task, not of the
  API: this one (judging whether a synopsis misleads, with no world
  knowledge) is hard.
- **Record `res.Model`.** Asking for `jev-latest` resolves to a release,
  and a threshold tuned on one release is not known to hold on the next.
- **Ask together.** Questions over one state go in one request: the
  API runs them in parallel, none sees another's answer, and the state's
  tokens are paid once. Question ids are not sent to the model, so each
  question must carry its whole meaning.
- **Refusals are typed.** A malformed request (no state, an unknown type,
  a Choice without `Options`, over 255 options, a Score outside 2–10
  levels, a criterion on the wrong type) is refused locally before any
  round trip. The API's refusals come back as `*claudia.JudgeError` with
  the HTTP status: 401 bad key, 422 bad request (the message names the
  field). 429 and 529 are retried with exponential backoff, honouring
  `Retry-After`, up to 3 retries. A response that leaves a question
  unanswered, answers the wrong type, or drops an option from a
  distribution is refused, not passed on with holes.
- **Key.** `TYPESAFE_API_KEY` in the environment, else the
  `TYPESAFE_API_KEY=` line of `~/.typesafe/env` (only that line is read;
  the file is not sourced). No key is `claudia.ErrJudgeNoKey`. The key
  never appears in an error.
- **Daemon.** With a daemon running, `Ask` goes through it and the
  daemon calls the API with its own key. Setting `APIKey`, `Endpoint` or
  `HTTPClient` on `JudgeConfig`, calling `SetDirect(true)`, or a daemon
  too old to know Judge, calls the API from your process instead. Both
  paths run the same code and return the same result.
- **Pricing and rate limits are not documented** by TypeSafe. No 429 was
  seen at 6 parallel requests. Measure your own budget.

## Session mode: essential patterns

```go
agent, err := claudia.Start(claudia.Config{
    WorkDir: "/abs/path",
    Model:   "opus",
})
defer agent.Stop()
```

`Start` returns as soon as the provider process has been spawned.
For Claude, that is before the TUI has finished painting its startup
UI. You do **not** need to sleep or poll: the first `Send` on a
Claude Session blocks internally until the TUI has gone quiet for
500 ms, which on a typical standalone session takes about 1.2 s from
`Start`. If you want to observe the ready transition (e.g. to update
a spinner), call `agent.WaitReady(ctx)` explicitly — it returns nil
once ready, or an error if detection gave up. Grok ACP and Codex
app-server readiness is protocol-level, not a tmux pane poll.

Subscribe to events **before** sending the first message — messages
may arrive quickly. Multiple subscribers are supported; each receives
every event independently:

```go
token := agent.SubscribeEvents(func(ev claudia.Event) {
    // ev.Type: "assistant", "user", "system", "progress", ...
    // ev.SessionID / ev.TurnID: backend-owned conversation and turn identity
    // ev.MessageID: backend message/item identity when available
    // ev.Text: concatenated text for assistant turns
    // ev.Usage: token counts (populated on assistant events)
    // ev.Raw:  complete JSONL line
})
defer agent.UnsubscribeEvents(token)

agent.Send("prompt")  // short text is typed; large payloads are pasted, followed by one typed line saying the operator sent the paste (Claude Code tells the model a bare paste may not be the user's words, and a seat refuses it). Extra Enters until the composer leaves idle. Failure is `turn not submitted: composer state=…` — never a silent "keys sent".
reply, err := agent.WaitForResponse(ctx)  // blocks until the turn's terminal stop_reason
```

**`WaitForResponse` always ends** (🎯T96). Three things can end it
besides the turn itself: the caller's context, the agent's death
(`ErrAgentGone` — a dead agent cannot publish the terminal event, so the
wait is unsatisfiable and says so at once), and silence
(`ErrTurnAbandoned` — nothing at all arrived, no event of any type and no
terminal byte, for `Config.TurnSilenceBound`). Both errors name the
session, the turn, what last arrived and how long ago. On a seat a
daemon holds, a silence during which the broker connection skipped
frames too large to relay also matches `ErrFramesDropped`: the turn's end
may have been the frame that was lost, so that silence is not evidence
the agent stopped (🎯T105).

The silence bound is on SILENCE, not on the turn: any activity rearms
it, so a turn that runs for hours is untouched. Raise
`Config.TurnSilenceBound` only for an agent whose healthy turns really do
go quiet for longer — a tool call that neither prints nor reports
progress for that long. Passing a context that never expires is now
safe; before this, it meant a goroutine parked for the life of the
process when a turn's terminal event went missing.

**A turn that finished first is still yours** (🎯T98). `Send` does not
always return before its turn can answer: a Cursor opening prompt blocks
until the peer has spoken, so the reply and its terminal event can both
be published while `Send` is still unwinding. `WaitForResponse` therefore
returns the turn the most recent `Send` submitted even when that turn
ended before the call — once, to one waiter. Events published before any
`Send` are not a turn anybody is waiting for (a resumed session replays
the old conversation), and a second wait with no `Send` between them
blocks for the next turn as it always did.

`TurnID` is safe to use directly as the grouping key for assistant and
progress events. Claudia never mints it from stop-reason observation: Claude
uses the prompt transcript record UUID, Codex uses its app-server turn ID, and
Grok uses its ACP prompt request ID. `TurnID == ""` means the backend did not
associate that event with an in-flight turn; consumers must not carry forward
the previous ID or manufacture a replacement.

For a one-shot, use the package-level helper:

```go
reply, err := claudia.Run(ctx, "prompt", cfg)
```

It bundles Start + Send + WaitForResponse + Stop.

**Interrupting**: `Interrupt()` cancels the current turn without
killing the process. Claude Session sends ESC to the PTY; Grok ACP
and Codex app-server use their interrupt RPCs.

### Steer, interrupt, queue: `SendMode` and `TurnCaps` (🎯T72.2)

`Send` conflates three host intents while a turn is open: start a turn,
hold text until the turn ends, or cancel the turn and start over. The
delivery API names them (design:
`docs/design/steer-interrupt-turn-api.md`):

```go
phase := agent.TurnPhase()          // TurnIdle | TurnInTurn (a reading, not a promise)
caps  := agent.TurnCaps()           // what THIS handle can do to an open turn

out, err := agent.SendMode(text, claudia.DeliverySteer)      // fold into the open turn
out, err  = agent.SendMode(text, claudia.DeliveryInterrupt)  // hard-stop, then submit
out, err  = agent.Steer(text)                                // steer only; ErrTurnIdle when idle
err        = agent.Send(text)                                // == SendMode(text, DeliverySubmit)
```

| Mode | Idle seat | Open turn |
|------|-----------|-----------|
| `DeliverySubmit` | starts a turn | the provider's own busy behaviour (`TurnCaps.BusyOnSecondSubmit`): Claude queues, ACP and Codex reject |
| `DeliverySteer` | plain submit | `Steer`: provider steer mechanism, or `ErrSteerUnsupported` |
| `DeliveryInterrupt` | plain submit | `Interrupt()` then submit; an interrupt failure aborts before any submit |
| `DeliveryQueue` | `ErrQueueHostSide` | `ErrQueueHostSide` — claudia holds no queue; nothing reaches the wire and the caller keeps the text |

`DeliveryOutcome` reports `Mode`, `PhaseBefore`, and `Mechanism` — what
actually ran (`submit`, `interrupt+submit`, `client_queue`,
`codex_turn_steer`, the ACP supersede label, or `steer_unsupported`) — plus
`SupersededTurnID` when an ACP steer pushed a second prompt over the
first. The outcome is also carried in its `Err` field so it can be logged
whole. `Mechanism` is a label for logs and UI, not a second source of
truth about the turn: events are.

**Per-provider `TurnCaps`** (`ProviderTurnCaps(p)` is the contract;
`agent.TurnCaps()` is what the live handle has wired, and is the one to
believe):

| Provider | Interrupt | Steer | `SteerPolicy` | Busy on second submit |
|----------|-----------|-------|---------------|-----------------------|
| Claude tmux | ESC | no | `queue_until_idle` — Claude Code queues the text itself | `queue` |
| Cursor ACP | `session/cancel` | second `session/prompt` supersedes (🎯T72.1) | `breakpoint` | `reject` |
| Grok ACP | `session/cancel` | second `session/prompt` (🎯T72.1) | `finish_slice` | `reject` |
| Codex app-server | `turn/interrupt` | `turn/steer` when the installed CLI lists it | `finish_slice`, else `queue_until_idle` | `reject` |
| Bedrock / Ollama | none (Task-only) | no | `none` | — |

`CanSteer` is true only when `Steer` reaches the wire. A provider whose
contract can steer but whose mechanism is not wired for this handle (the
ACP seam before 🎯T72.1 lands; a Codex CLI whose
`app-server generate-json-schema` dump has no `turn/steer`) reports
`CanSteer=false, SteerPolicy=queue_until_idle`, and `Steer` returns
`ErrSteerUnsupported` with `Mechanism=steer_unsupported`. Codex is probed
once per binary path at `Start`. A plain `Send` while an ACP or Codex
turn is open is refused by the provider (the ACP clients wrap that as
`ErrTurnInFlight`, 🎯T72.1); the host queues — claudia never auto-steers
a submit.

`TurnPhase` reads the provider's prompt-in-flight signal (Claude: the
pane; ACP and Codex: the open RPC). Unknown reads as idle, so
`DeliveryInterrupt` on a phase the handle cannot see is a plain submit
rather than a stray hard-stop.

**Testing a host path**: `NewStubAgentOps(&StubAgentOps{Send, Steer,
Interrupt, TurnPhase, TurnCaps})` builds an alive stub whose every verb
is observable, so a dependent package can assert which of
Send / Steer / Interrupt its broker or composer path ran, on the same
`SendMode` path the product uses.

**Terminal output**: Claude Session mode captures the raw PTY byte
stream to a log file at
`$XDG_STATE_HOME/claudia/terms/<escaped-workdir>/<sessionID>.term`
(defaulting to `~/.local/state/...` when the XDG var is unset).
Override via `Config.TermLogPath`; set to `"-"` to disable. This file
contains ANSI escapes, cursor moves, and progress bars — it is the
rendered terminal view, not a structured feed. The JSONL transcript
is authoritative for logical content. Grok and Codex Session have no
PTY log.

**Live terminal streaming**: `SubscribeTerminal()` returns the
buffered history and a live channel of PTY chunks. Always call
`UnsubscribeTerminal(ch)` when done. Subscribers that don't drain
their channel drop data (sends are non-blocking).

**Provisional TUI preview (Claude Session)**: while a turn is open,
Claude Session publishes `Type=progress` Events with
`ProgressType=tui_preview`. `Text` is **generated Markdown** from a full
scrape parse under open `⏺` blocks (blank-line paragraphs with soft-wrap
join; box-drawing tables as pipe Markdown, truncated at the last complete
row while the grid is still painting) — not raw pane glyphs. Tool chrome
(`⏺ Bash(...)`, `Ran N shell command(s)`) is excluded.

`Event.PreviewUpdate` tells every client how to apply provisional text
without branching on provider:
- `append` — `Text` is a suffix delta to append to the open buffer
- `rewrite` — `Text` replaces the open buffer wholesale

Claude derives the kind by prefix-comparing generated Markdown snapshots
(scrape reflows become `rewrite`, not errors). Grok/Cursor ACP
`agent_message_chunk` events set `PreviewUpdate=append` on
`Type=assistant` chunks so the same client rule applies to true streams.
When the matching assistant-text JSONL lands, a normal `Type=assistant`
Event (no `PreviewUpdate`) seals that block — replace the provisional
blob with `Event.Text`. `WaitForResponse` ignores preview (and
preview-fault) Events; JSONL remains the completion path. Preview is
UX-only: not billed, not durable reply.

Scrape chrome assumptions still use checked invariants
(`ran_shell_chrome_form`). Open-block scroll-off is trimmed silently
(viewport capture has no scrollback). Failures publish
`ProgressType=tui_preview_fault` with invariant id, detail, and a bounded
pane excerpt (also `slog.Error`). That usually means Claude Code TUI
chrome changed.

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
treats the synthetic message as a normal reply.

**Switching models mid-session (same provider)**: call
`agent.SetModel("sonnet")` (or any provider-native id/alias). This is
gated by the `model_switch` capability: Claude types `/model <name>` into
the live TUI; Codex applies the model on the next `turn/start`; Grok and
Cursor use ACP `session/set_config_option` (legacy `session/set_model`
fallback). SetModel refuses an empty name, a dead agent, or a turn still
in flight. On success it publishes a `Type=system` Event carrying the
requested `Model` and `Agent.Model()` updates immediately (a later
assistant event may refine it to a fully resolved id). Task-only
providers (Bedrock, Ollama) refuse. Switching *providers* on a live
conversation uses [Agent.Migrate] (🎯T55), not SetModel.

**Switching providers mid-session**: `agent.Migrate(&claudia.MigrateArgs{Provider: claudia.ProviderGrok, Model: "grok-4"})`.
Gated by the `migrate` capability: supported for Claude, Codex, Grok, and Cursor Sessions; Task-only Bedrock and Ollama refuse with `*CapabilityError`. Same-provider calls are refused (use SetModel). Claudia never auto-migrates — the host decides when.

The destination is a **new native session** (`session/new` / `--session-id`). Claudia never `--resume` or `session/load` the predecessor's id on the destination. `Agent.SessionID()` rotates; the same `*Agent` handle and `SubscribeEvents` subscriptions stay valid.

Continuity is an **inert distilled seed**, not a verbatim transcript: goal, last recoverable user request, relevant files, work completed / still open, stopping point, and warning codes (for example `stale_tool_output`). Foreign system prompts, thinking, and tool calls are not replayed as executable. Missing both last-user and last-assistant refuses unless `Force` (cold) is set. The seed is sent as the destination's first user turn, wrapped so transcript text cannot become instructions.

On success, a `Type=system` Event with `ProgressType=model_switch` is published **before** the destination accepts work. It carries `FromProvider`, `ToProvider`, `FromModel`, `Model` (destination), `Reason` (host-supplied or `"explicit"`), the post-migrate `SessionID`, and `WarningCodes` when the seed was partial. Switches are never silent.

When a turn ends in a classified stuck/exhausted condition (`rate_limit` or spend/quota wall — not auth, not `model_not_found`), Claudia publishes `ProgressType=stuck` with `StuckClass` and a short detail on the same stream and does **not** SetModel, Migrate, remint, or climb a ladder. The host decides the next step.

Resumed-prefix disk reconstruction (compact/snip/Codex rollback) is a follow-on (🎯T55.2 / 🎯T56). In-flight migrate seeds from the retained live Event log.

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

A seat a `Registry` holds is rewound with `reg.Rewind(ctx, name, n)`,
which stops, truncates and relaunches under the seat's reservation (a
racing `Launch` waits rather than starting the old conversation) and
keeps the Registry's handle current. On a daemon-held seat both calls
go to the daemon, and the returned `*Agent` is the same handle,
re-pointed, with its subscriptions.

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
to add definitions and `Launch` / `StartAll` to launch them. `Launch`
never reaps leftover windows (a leak stays visible). `Adopt` /
`AdoptOrLaunch` / `StartAllPreferAdopt` reuse a leftover window
without spawning or reaping. Drain is `StopAll`. If the host program
owns a single short-lived agent, skip the Registry.

`StartContext`, `LaunchContext`, `AdoptOrLaunchContext` and
`StartAllPreferAdoptContext` accept startup cancellation. The context does
not own the returned agent's lifetime. Cursor ACP startup is interruptible;
other providers cooperate where supported and may finish after cancellation.
Provider operations run outside the registry's global lock, with one lifecycle
reservation per name. `Stop` / `Remove` cancel and join pending startup before
returning, including cleanup of late results. Metadata-only `Register` updates
remain allowed during startup; conflicting process configuration returns
`ErrLifecycleInProgress` and should be retried after the operation finishes.

## Daemon: `claudia broker` (optional, host-wide)

`claudia broker serve` is a long-running process that owns agent
lifecycles for every claudia consumer on the host (🎯T2). Nothing in
the API above changes when it runs; what changes is who owns the
process.

### Supported path

The supported path, when a daemon is up, is the Go library holding one
`*Agent`. The default socket is `~/.local/state/claudia/broker.sock`
(`$XDG_STATE_HOME/claudia/broker.sock` when that variable is absolute;
`CLAUDIA_BROKER_SOCKET` overrides; `claudia broker socket` prints it).
`BrokerAvailable` reports whether a daemon is behind that socket.

Leave `CLAUDIA_NO_BROKER`, `Registry.SetDirect`, `Task.SetDirect`, and
`StartDirect` unset. Those select the in-process path. The handle's
connection stays open for the life of the `*Agent`: that connection
owns the grant and is where events arrive. `Send`, `Steer`,
`Interrupt`, `WaitForResponse`, `Stop`, and `Detach` run on it.
`WaitForResponse` is a client-side fold over that stream (assistant
text through a terminal stop, then a short settle). The wire has no
`wait` message. `Stop` releases the seat and tears it down. `Detach`
releases ownership and leaves the seat running; a later `Start` or
`Launch` of the same name reclaims it.

`Config.Name` is the grant key:

```go
agent, err := claudia.Start(claudia.Config{
    Name:     "pimp-smoke-1",
    Provider: claudia.ProviderGrok,
    WorkDir:  workDir,
})
if err != nil { /* ... */ }
defer agent.Stop()
if err := agent.Send("Reply with exactly the word pong."); err != nil { /* ... */ }
text, err := agent.WaitForResponse(ctx)
```

Parent and Purpose are fleet labels on `AgentDef`, not fields of
`Config`. Empty Purpose means `work`. They ride the grant when a
consumer Registry launches the seat. An ephemeral Pimp seat uses
Parent `pimp`, a name like `pimp-smoke-*`, and Purpose `work`, `aside`,
or `overseer`:

```go
err = reg.Register(claudia.AgentDef{
    Name:      "pimp-smoke-1",
    Parent:    "pimp",
    Purpose:   claudia.PurposeWork,
    Provider:  claudia.ProviderGrok,
    WorkDir:   workDir,
    SessionID: uuid.NewString(),
})
agent, err = reg.Launch("pimp-smoke-1")
// Send, Steer, Interrupt, WaitForResponse, Stop, and Detach, same as Start.
```

The registry file is the consumer's. The daemon keeps its own grants.
`Launch` sends the definition over the socket.

A daemon installed as brew 0.44.0 already speaks this grant. Library
callers smoke it against `~/.local/state/claudia/broker.sock` today.
The `grant` / `send` / `interrupt` / `events` CLI subcommands are on
HEAD; that brew `claudia` binary does not have them yet, so smoke the
CLI with a HEAD build against the same socket until the formula
catches up. `claudia broker grant -h` lists the flags. One shell
smoke, which holds one connection the way the `*Agent` does:

```bash
claudia broker grant \
  --name "pimp-smoke-$RANDOM" --provider grok --workdir "$PWD" \
  --parent pimp --purpose work \
  --send 'Reply with exactly the word pong.' --wait --release stop
```

Stdout is the assistant text. Exit without `--release stop` detaches
(the seat keeps running). A later `send`, `interrupt`, or `events`
re-grants from the fields `grants` lists and keeps the daemon's session
id. A seat that also carries MCP servers or a sandbox policy stays on
the library, which re-grants the definition it first sent.

- **Sessions are grants.** `Start` / `Registry.Launch` send the
  Config (as an `AgentDef` plus `Config.Name`) over the Unix socket;
  the daemon starts the provider process as its parent and streams
  `Event`s back. The returned `*Agent` is a handle: `Send`,
  `Interrupt`, `WaitForResponse`, `SetModel`, `Migrate`, `Usage`,
  `SubscribeTerminal` all work (`SendMode` / `Steer` / `TurnCaps` ride
  the `send.mode` wire, 🎯T72.3); `JSONLPath` / `AttachCommand` name
  host-local paths the daemon reported. `Rewind` rewinds and
  relaunches the seat on the daemon and re-points the handle.
- **Seats outlive the consumer.** If the consumer exits or crashes,
  the seat keeps running unowned and retains the in-flight stream (up
  to 100000 events) so a consumer bounce does not drop a live turn. A new
  process that `Start`s or `Launch`es the same `Config.Name` reclaims
  it — same session, history replayed ahead of live events — for
  every Session provider, including the stdio ones (Codex, Cursor)
  that die with their parent on the direct path. A second live
  consumer asking for a held name gets `grant_held`; nobody steals a
  seat. `Agent.Alive()` goes false when the daemon connection is lost,
  so a consumer's existing "not alive → relaunch" path is the
  reconnect.
- **Tasks run on the daemon.** `Task.Run` streams the run over its
  own connection; `Cancel` reaches it; a dropped connection cancels
  the run. `Task.SetRawLog` receives the provider's raw lines from the
  daemon, in order; without it they stay on the daemon.
- **Judge runs on the daemon.** `Judge.Ask` sends the request over its
  own connection and the daemon calls TypeSafe with its key; a caller
  that set its own key, endpoint or HTTP client stays direct.
- **Plan usage is the daemon's.** `LoadPlanUsage` and `Resolve` read
  the daemon's snapshot; the daemon refreshes on a TTL and immediately
  when any seat reports a rate limit or quota stop. The filesystem
  cache is the path when no daemon runs.
- **After a host reboot** the daemon brings back every seat it held:
  adopts what still runs (tmux, connect-mode), relaunches the rest
  with session resume, and sends a relaunched seat a restart nudge
  (`--restart-nudge`; `-` disables) so it picks its work back up.
- **Direct mode has the same capabilities.** No socket, a socket with
  no daemon runtime behind it (`not_available`), `CLAUDIA_NO_BROKER=1`
  for the whole process, or `Registry.SetDirect(true)`,
  `Task.SetDirect(true)` and `StartDirect` for one Registry, Task or
  start, is the in-process path. Everything that does not imply a server
  is library code the daemon passes through (🎯T75): seat resume
  (`Registry.ResumeAll`), seat lifecycle events
  (`Registry.SubscribeSeatEvents`), `Registry.Rewind`, the plan-usage
  monitor (`PlanUsageMonitor`), MCP hosting (`MCPHost`), and the
  model-intel refresher (`RunModelIntelRefresher`). What only a daemon
  gives is a seat that outlives its consumer (`Agent.Detach` lets go of
  one for a later reclaim), one seat owner at a time, one usage evaluator
  for the host, and the socket. Auto-actuating policy (rebind 🎯T2.12,
  reaping, preemption) will be opt-in library code, off in direct mode
  unless enabled; none is built yet.

**Test suites must opt out.** A running daemon is reachable from
`go test` like from any process, so a consumer's hermetic suite that
launches agents through the Registry would be granted real seats with
real provider processes behind them. Set `CLAUDIA_NO_BROKER=1` in the
suite (a `TestMain`, or the Makefile test rule); claudia's own suite
does. Tests that want a daemon start one on a temp socket
(`daemon.New` from `github.com/marcelocantos/claudia/daemon`, with
`SocketPath`) and re-enable the consult with
`t.Setenv("CLAUDIA_NO_BROKER", "")`. A test that needs agents without
provider processes starts them with `claudia.StartStub` (the real Start
machinery with stub verbs) or `claudia.NewStubTask`, and a Registry
launches stubs with `Registry.SetLaunchers`.

Install the daemon with `brew install marcelocantos/tap/claudia`.
On a supervisor-hosted machine, `make supervisor-install` (or
`supervisor/install.sh`) renders `supervisor.d/claudia.ini`, evicts
brew/launchd, and starts `claudia broker serve` under supervisord —
same shape as bullseye/mnemo/jevonsd. Elsewhere, operate it with
`brew services start claudia` (Homebrew launchd plist, 🎯T2.7) or
`claudia broker install` (owner-installed launchd user agent on
macOS). Operator commands: `status`, `grants`, `usage [--refresh]`,
`tail` (NDJSON lifecycle events), `release NAME [--detach]`, `socket`.
Seat driving from the shell is the HEAD CLI in the supported-path
section above. `claudia --help-agent` prints this guide after the CLI
usage text.

`Acquire` draws from a pool the daemon runs: every consumer on the
host shares its warm windows, `Agent.Release` returns or drops the seat
on the daemon, and a consumer that goes away returns what it held.
`AcquireDirect` keeps the pool in-process. A pooled agent publishes
turn events on either path, so `WaitForResponse` and `SubscribeEvents`
work on it exactly as on a `Start`-ed one; a window returned and
acquired again gives its new holder that holder's own turns and none
of the previous holder's (🎯T78). A seat's
Goal loop runs on the daemon; after a terminal turn with no
`GOAL_STATUS` line it asks the owning handle's `GoalCompleteCheck`
(set on `Config` or with `SetGoalCompleteCheck`), and with no owner
connected, no check, or no answer within 30s it continues as if the
check said not complete.
Design record: [docs/metaharness.md](docs/metaharness.md).

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
   then known locations including `~/.grok/bin/grok`. Ollama needs a
   reachable daemon (`CLAUDIA_OLLAMA_ENDPOINT`, default
   `http://127.0.0.1:11434`) and a model (`TaskConfig.Model` or
   `CLAUDIA_OLLAMA_MODEL`). Bedrock uses the AWS SDK default chain.

   Current provider capability matrix (full table: [STABILITY.md](STABILITY.md)):

   | Capability | Claude | Codex | Grok | Bedrock | Ollama |
   |------------|--------|-------|------|---------|--------|
   | Task | Supported | `codex exec --json` | `grok -p` streaming-json | ConverseStream | `/api/generate` |
   | Session | tmux PTY | `codex app-server` | ACP stdio/serve | Unsupported | Unsupported |
   | Resume | Supported | `codex exec resume` | `--resume` / ACP load | Unsupported | Unsupported |
   | Rewind | Supported | Unsupported | Unsupported | Unsupported | Unsupported |
   | Cost | Supported | tokens only | unsupported | unsupported | unsupported (latency, not money) |
   | tmux attach / terminal log | Supported | Unsupported | Unsupported | Unsupported | Unsupported |
   | Permission mode | Supported | Unsupported (Codex-native sandbox, not a mapping) | Unsupported (Task hardcodes bypassPermissions) | Unsupported | Unsupported |
   | Tool restrictions | Supported | Unsupported — `Task.Run` **refuses** | Unsupported — `grok` has `--deny` but claudia does not translate; **refuses** | Unsupported — **refuses** | Unsupported — **refuses** |
   | Sandbox policy | Unsupported | Supported (`SandboxMode` / `ApprovalPolicy`) | Unsupported — **refuses** | Unsupported — **refuses** | Unsupported — **refuses** |
   | Extra args | Supported | Unsupported — **refuses** | Unsupported — **refuses** | Unsupported — **refuses** | Unsupported — **refuses** |
   | Image inputs | Unsupported (no claudia API) | Unsupported | Unsupported | Unsupported | Unsupported |
   | Model switch | Supported (`/model`) | Supported (next turn/start) | Supported (ACP) | Unsupported | Unsupported |
   | Migrate (inter-provider) | Supported | Supported | Supported | Unsupported | Unsupported |
   | Web search | Supported | Unsupported (does not bind `--search`) | Unsupported | Unsupported | Unsupported |

   Cursor is a sixth Session provider (ACP, like Grok): Task, Session,
   Resume, model switch, and migrate are Supported; Rewind, ExtraArgs,
   sandbox, and tool restrictions refuse. Query `ProviderCapabilityMatrix`
   rather than this abbreviated table for Cursor.

   This table is generated from the same claims production reads. Query
   it with `claudia.ProviderCapabilityMatrix(provider)`, or gate one
   call with `claudia.CheckCapability(provider, capability)`, which
   returns the `*claudia.CapabilityError` the operation would return.
   Unknown providers and unclaimed capabilities report
   `CapabilityUnsupported` — silence never reads as parity with Claude.

2. **Sub-agents are disabled — on Claude.** Claude Session and Task
   modes always pass
   `--disallowedTools Agent,SendMessage,EnterWorktree`.
   The host Go program owns the process lifecycle; nested claudia
   sessions would fight over PTY ownership and transcript tailing.
   Don't try to re-enable these.

   Those are Claude Code tool names, and `BaseDisallowedTools` is
   applied on Claude only — never on Codex, Grok, Cursor, Bedrock, or
   Ollama. Rather than pretend otherwise, the non-Claude providers
   report `CapabilityToolRestrictions` as unsupported, and a Codex,
   Grok, or Cursor task carrying `DisallowTools` is refused outright
   rather than run with the restriction dropped (see the matrix above).

   The same rule applies to every caller-supplied field: a path that
   cannot honour it returns `*CapabilityError` (Grok Session also
   refuses non-bypass `PermissionMode` and `ExtraArgs`). There is no
   silent drop.

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

3. **Session resumption is automatic on `Start`.** `Start` checks
   whether `<SessionID>.jsonl` exists under Claude Code's project
   directory. If it does, claudia passes `--resume`; otherwise
   `--session-id`. Pass a stable `SessionID` to get resumption for
   free. **`Migrate` is the opposite:** the destination is always a
   new native session (`Resuming=false`). Claudia never `--resume` or
   `session/load` the predecessor's id on the destination provider.

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

5. **Don't stack `WaitForResponse` concurrently on the same agent.**
   It subscribes its own event listener and unsubscribes on return;
   it does not replace other subscribers. Concurrent waits on one
   Agent still race on "whose turn ended," and a turn that completed
   before either of them subscribed goes to whichever gets there first.

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

Claude Session mode agents run inside windows on a dedicated claudia
tmux server (socket at `$XDG_STATE_HOME/claudia/tmux.sock`, defaulting
to `~/.local/state/claudia/tmux.sock`). The server starts
automatically on the first Claude `Start` or `Acquire` call — no
launchd or systemd setup is needed. Grok and Codex Session do not use
tmux.

### Human observability: AttachCommand

Every agent exposes `AttachCommand()` which returns the exact tmux
invocation to attach to its window:

```go
fmt.Println(agent.AttachCommand())
// e.g. tmux -S ~/.local/state/claudia/tmux.sock attach -t @3
```

Run that command from a terminal to watch the live Claude Code TUI.
This is the primary debugging tool when an agent is misbehaving.

### Session-chain tracker

`RegisterChain` / `LookupChain` persist session-id chains on the
filesystem. That tracker is not a `claudiad` sidecar; the optional
host-wide process is `claudia broker serve`.

## grok subpackage

`github.com/marcelocantos/claudia/grok` is a Grok Realtime voice API
client. It is independent of the rest of claudia — a separate concern
that happens to live in the same module because the original use case
was voice-driving a claudia agent. If you're integrating voice +
Claude Code, wire `grok.Config.OnFunctionCall` to a `claudia.Task`
`Run` invocation and relay results via `InjectAssistantText`.
Otherwise, ignore it.

## Testing

The test suite has two tiers.

**Hermetic tests are the default.** They run anywhere — no provider
binary, no credentials, no API cost. CI runs `go test -race -count=1
./...` on every push. Use them for parsers, capability refusals,
lifecycle, and anything a fake peer can decide.

**Live tests are a hard gate for backend changes.** Hermetic tests
cannot decide spawn, submit, auth, or turn-loop behaviour. When you
change how a provider is started, spoken to, or observed (`Start`,
`Send`, `WaitForResponse`, Goal continuation, event mapping, sandbox,
auth, app-server/ACP/exec/tmux framing), run the live tests for
**every backend whose wire you touched**. A Session-wide change is
every Session backend you can authenticate — not just the one you
had in mind. CI never sets these gates. A skipped live test is not
a pass; name it as residue. Full rule: [`AGENTS.md`](AGENTS.md).

Do not use live tests as the everyday suite, and do not retire a
target on live smoke *alone* — hermetic journeys still have to
exist. Do not retire on hermetic green *alone* either.

| Gate | Surfaces | Named live tests |
|------|----------|------------------|
| `CLAUDIA_LIVE=1` | Claude Task + Session | `TestTaskRunSmoke`, `TestAgentSendAndWaitForResponse`, `TestGoalJourneyLiveBackends/claude` |
| `CLAUDIA_GROK_LIVE=1` | Grok Task + Session | `TestGrokTaskRunSmoke`, `TestGrokSessionLiveSmoke`, `TestGoalJourneyLiveBackends/grok` |
| `CLAUDIA_CODEX_LIVE=1` | Codex Task + Session | `TestCodexTaskRunSmoke`, `TestCodexSessionLiveSmoke`, `TestGoalJourneyLiveBackends/codex` |
| `CLAUDIA_BEDROCK_LIVE=1` | Bedrock Task | `TestBedrockTaskLiveSmoke` |
| `CLAUDIA_OLLAMA_LIVE=1` | Ollama Task | `TestOllamaTaskLiveSmoke` (needs `CLAUDIA_OLLAMA_MODEL`) |
| `CLAUDIA_CURSOR_LIVE=1` | Cursor Task + Session | `TestCursorTaskLiveSmoke`, `TestCursorSessionLiveSmoke`, `TestGoalJourneyLiveBackends/cursor`, `TestMCPLiveLoadAndSessionSeesMnemo/cursor`, `TestMCPExclusiveCursorSessionRoundTrip` |

```sh
# one backend you just touched
CLAUDIA_CODEX_LIVE=1 make live

# every Session backend you have authed locally
CLAUDIA_LIVE=1 CLAUDIA_GROK_LIVE=1 CLAUDIA_CODEX_LIVE=1 \
  CLAUDIA_CURSOR_LIVE=1 CLAUDIA_BEDROCK_LIVE=1 CLAUDIA_OLLAMA_LIVE=1 make live
```

Unset gates skip. CI does not set any of them. Bedrock needs
work-account AWS credentials ([docs/bedrock-work-account.md](docs/bedrock-work-account.md)).

## Stability

claudia is pre-1.0. `STABILITY.md` in the repo root tracks the public
interaction surface and flags which parts are stable, under review,
or still fluid. Consult it before building consumers that assume long
term API stability.

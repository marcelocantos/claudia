# Stability

claudia is pre-1.0. This document tracks the project's readiness for
a 1.0 release.

## Stability commitment

1.0 represents a backwards-compatibility contract. After 1.0,
breaking changes to the public Go API require a major version bump,
and per project policy a major bump means forking the library into a
new module (e.g. `claudia2`) rather than breaking an existing import
path. The pre-1.0 period exists to shake out the API design before
that contract takes effect.

Snapshot as of: v0.21.0 (tagged 2026-08-10).

> **Present at HEAD, not yet released:** `ProviderOllama` landed after
> v0.21.0. The snapshot tracks releases, so it is enumerated by the
> release that ships it, not here.

## Interaction surface

The exhaustive list of public-facing items in the module at the snapshot
tag, derived from `go doc -all` over a clean `v0.21.0` worktree rather
than transcribed from the previous revision of this document. Items are
listed alphabetically within each table so the list can be diffed against
`go doc` mechanically. Each item is annotated with a stability assessment:

- **Stable** — unlikely to change. Design is settled.
- **Needs review** — functional but may benefit from refinement
  before locking in.
- **Fluid** — actively evolving or known to need rework.

`make verify-stability` (CI job `stability-surface`) re-derives the surface from
a clean worktree of the snapshot tag and fails if this document names an item
the tag does not have, omits one it does — type, function, method, constant,
variable, struct field, or environment variable — leaves a row unassessed, or
states a count that does not match. The document cannot drift silently from the
release it claims to describe.

### Package `github.com/marcelocantos/claudia`

#### Types

| Item | Definition | Status |
|---|---|---|
| `Agent` | opaque struct; methods listed below | Needs review |
| `AgentDef` | struct with `Name, WorkDir, SessionID, Model, Parent, Purpose, Description, TargetID, ConnectURL string`, `Provider Provider`, `DisallowTools []string`, `AutoStart, Materialized, GrokConnect bool`, `ConnectPID int` | Needs review |
| `AllPlanUsageArgs` | struct: every `PlanUsageArgs` field except `Provider`, plus `Providers []Provider` (empty means all supported providers) | Fluid |
| `Capability` | string type naming a reported provider behaviour | Fluid |
| `CapabilityError` | struct with `Provider Provider`, `Capability Capability`, `Status CapabilityStatus`, `Reason string`; method `Error() string` | Fluid |
| `CapabilityStatus` | string type: `CapabilitySupported`, `CapabilityUnsupported`, `CapabilityExperimental` | Fluid |
| `CodexAuthMode` | string type: `CodexAuthModeChatGPT`, `CodexAuthModeAPIKey`, `CodexAuthModeUnknown` | Fluid |
| `CodexAuthPreflight` | struct with `Mode CodexAuthMode`, `AuthPath, Reason string`, `HasAccessToken, HasAPIKeyInFile, EnvOpenAIAPIKey, SubscriptionOK bool`, `Warnings []string` | Fluid |
| `CodexAuthPreflightArgs` | struct with `AuthPath string`, `Getenv func(string) string` | Fluid |
| `Config` | struct with `Provider Provider`, `WorkDir, SessionID, Model, PermissionMode, MCPConfig, TermLogPath, PoolPolicy, ConnectURL string`, `RequireResume, GrokConnect bool`, `ExtraArgs, DisallowTools []string`, `PoolCap, ConnectPID int` | Needs review |
| `Event` | struct with `Type string`, `Raw []byte`, `Text, StopReason, ProgressType, Model string`, `Usage Usage`, `IsError bool`; method `IsTerminalStop() bool` | Stable |
| `EventFunc` | `func(Event)` | Needs review |
| `PlanUsage` | struct with `Provider Provider`, `Status PlanUsageStatus`, `Reason, PlanType string`, `Windows []PlanWindow`, `FetchedAt time.Time` | Needs review |
| `PlanUsageArgs` | struct with `Provider Provider`, `HTTPClient *http.Client`, `Now time.Time`, credential/endpoint overrides `ClaudeAccessToken, ClaudeUsageURL, CodexAccessToken, CodexAccountID, CodexAuthPath, CodexUsageURL, GrokAccessToken, GrokAuthPath, GrokBillingURL string`, `GrokBillingRaw json.RawMessage`, `GrokUnstableUsage bool` | Fluid |
| `PlanUsageStatus` | string type: `PlanUsageAvailable`, `PlanUsageUnavailable` | Needs review |
| `PlanWindow` | struct with `Name PlanWindowName`, `UsedPercent, RemainingPercent *float64`, `ResetsAt *time.Time`, `LimitWindow time.Duration` | Needs review |
| `PlanWindowName` | string type: `PlanWindowSession`, `PlanWindowWeekly` | Needs review |
| `Provider` | string type selecting `ProviderClaude`, `ProviderCodex`, `ProviderGrok`, or `ProviderBedrock` | Fluid |
| `RawLogFunc` | `func(line []byte)` | Stable |
| `Registry` | opaque struct; methods listed below | Needs review |
| `RewindResult` | struct with `SessionID, JSONLPath, BackupPath string`, `TurnsRemoved, LinesRemoved int`, `BytesRemoved int64` | Needs review |
| `Task` | opaque struct; methods listed below | Needs review |
| `TaskConfig` | struct with `Provider Provider`, `ID, Name, WorkDir, Model, ClaudeID, LastResult, SandboxMode, ApprovalPolicy string`, `DisallowTools []string` | Needs review |
| `TaskEvent` | struct with `Type TaskEventType`, `Content, ToolName, ToolInput, ToolID, SessionID string`, `DurationMs, CostUSD float64`, `Usage Usage`, `IsError bool`, `ErrorMsg string`, `Model string` | Needs review |
| `TaskEventType` | string type | Stable |
| `TaskStatus` | string type | Stable |
| `Usage` | struct with `InputTokens, OutputTokens, CacheCreationInputTokens, CacheReadInputTokens int` | Stable |

#### Constants

| Item | Status |
|---|---|
| `BaseDisallowedTools` | Needs review |
| `CapabilitySupported, CapabilityUnsupported, CapabilityExperimental` (CapabilityStatus) | Fluid |
| `CapabilityTask, CapabilitySession, CapabilityResume, CapabilityRewind, CapabilityCost, CapabilityTmuxAttach, CapabilityTerminalLog, CapabilityPermissionMode, CapabilityToolRestrictions, CapabilityImageInput, CapabilityWebSearch` (Capability) | Fluid |
| `CodexAuthModeChatGPT, CodexAuthModeAPIKey, CodexAuthModeUnknown` (CodexAuthMode) | Fluid |
| `EnvGrokConnect` (the name of the `CLAUDIA_GROK_CONNECT` env var) | Fluid |
| `PlanUsageAvailable, PlanUsageUnavailable` (PlanUsageStatus) | Needs review |
| `PlanWindowSession, PlanWindowWeekly` (PlanWindowName) | Needs review |
| `ProviderClaude, ProviderCodex, ProviderGrok, ProviderBedrock` (Provider) | Fluid |
| `PurposeWork, PurposeAside, PurposeOverseer` (untyped string, for `AgentDef.Purpose`) | Needs review |
| `TaskEventInit, TaskEventText, TaskEventToolUse, TaskEventResult, TaskEventError` (TaskEventType) | Stable |
| `TaskStatusIdle, TaskStatusRunning, TaskStatusError, TaskStatusStopped` (TaskStatus) | Stable |
| `Version` | Stable |
| ~~`ErrDaemonUnavailable`~~ | Removed (daemon pivot) |

#### Functions

| Item | Signature | Status |
|---|---|---|
| `Acquire` | `Acquire(ctx context.Context, cfg Config) (*Agent, error)` | Needs review |
| `CheckCapability` | `CheckCapability(provider Provider, capability Capability) error` | Fluid |
| `LookupChain` | `LookupChain(sessionID string) (chainID string, sessionIDs []string, err error)` | Needs review |
| `NewRegistry` | `NewRegistry(path string) (*Registry, error)` | Stable |
| `NewTask` | `NewTask(cfg TaskConfig) *Task` | Stable |
| `ParseTaskLine` | `ParseTaskLine(line []byte) []TaskEvent` | Stable |
| `PreflightCodexAuth` | `PreflightCodexAuth(args *CodexAuthPreflightArgs) CodexAuthPreflight` | Fluid |
| `ProviderCapabilityMatrix` | `ProviderCapabilityMatrix(provider Provider) map[Capability]CapabilityStatus` | Fluid |
| `ProviderCapabilityReason` | `ProviderCapabilityReason(provider Provider, capability Capability) string` | Fluid |
| `ProviderCapabilityStatus` | `ProviderCapabilityStatus(provider Provider, capability Capability) CapabilityStatus` | Fluid |
| `QueryAllPlanUsage` | `QueryAllPlanUsage(ctx context.Context, args *AllPlanUsageArgs) ([]PlanUsage, error)` | Fluid |
| `QueryPlanUsage` | `QueryPlanUsage(ctx context.Context, args *PlanUsageArgs) (PlanUsage, error)` | Fluid |
| `RegisterChain` | `RegisterChain(chainID, sessionID string) error` | Needs review |
| `RewindSession` | `RewindSession(sessionID, workDir string, n int) (*RewindResult, error)` | Needs review |
| `Run` | `Run(ctx context.Context, prompt string, cfg Config) (string, error)` | Stable |
| `SessionExists` | `SessionExists(sessionID, workDir string) (bool, error)` | Needs review |
| `SessionJSONLPath` | `SessionJSONLPath(sessionID, workDir string) string` | Needs review |
| `Start` | `Start(cfg Config) (*Agent, error)` | Needs review |
| `Unrewind` | `Unrewind(path string) error` | Needs review |

#### `Agent` methods

| Item | Signature | Status |
|---|---|---|
| `Alive` | `() bool` | Stable |
| `AttachCommand` | `() string` | Needs review |
| `ConnectURL` | `() string` — Grok connect-mode reattach URL, `""` otherwise | Fluid |
| `EventSubscriberCount` | `() int` — hermetic oracle for fan-out idempotency | Needs review |
| `Interrupt` | `() error` | Stable |
| `JSONLPath` | `() string` | Stable |
| `Model` | `() string` | Needs review |
| `PID` | `() int` — durable process id (Grok connect-mode serve); 0 for stdio children and Claude tmux | Fluid |
| `ProcessAlive` | `() bool` — falls back to `Alive` when no PID is known | Fluid |
| `PromptInFlight` | `() bool` — provider turn open and blocking `Send`; false when unknown | Fluid |
| `PublishEvent` | `(ev Event)` — drives the subscriber fan-out without a live JSONL tail | Needs review |
| `Release` | `(disposition string) error` | Needs review |
| `Resize` | `(cols, rows uint16) error` | Stable |
| `Rewind` | `(n int, cfg Config) (*Agent, error)` | Needs review |
| `Send` | `(msg string) error` | Stable |
| `SessionID` | `() string` | Stable |
| `Stop` | `()` | Needs review |
| `SubscribeEvents` | `(fn EventFunc) int64` | Needs review |
| `SubscribeTerminal` | `() (history []byte, ch chan []byte)` | Needs review |
| `TermLogPath` | `() string` | Needs review |
| `UnsubscribeEvents` | `(token int64)` | Needs review |
| `UnsubscribeTerminal` | `(ch chan []byte)` | Needs review |
| `Usage` | `() Usage` | Needs review |
| `WaitForResponse` | `(ctx context.Context) (string, error)` | Needs review |
| `WaitReady` | `(ctx context.Context) error` | Stable |

#### `Task` methods

| Item | Signature | Status |
|---|---|---|
| `Cancel` | `() error` | Stable |
| `ClaudeID` | `() string` | Stable |
| `ID` | `() string` | Stable |
| `LastResult` | `() string` | Stable |
| `Model` | `() string` | Needs review |
| `Name` | `() string` | Stable |
| `Run` | `(ctx context.Context, prompt string) (<-chan TaskEvent, error)` | Stable |
| `SetRawLog` | `(fn RawLogFunc)` | Stable |
| `Status` | `() TaskStatus` | Stable |
| `Stop` | `()` | Stable |
| `WorkDir` | `() string` | Stable |

#### `Registry` methods

| Item | Signature | Status |
|---|---|---|
| `Def` | `(name string) *AgentDef` | Stable |
| `Descendants` | `(root string) []string` — depth-first subtree walk, excluding `root` | Needs review |
| `EnsureAgent` | `(name, workDir, model string, autoStart bool) (*AgentDef, error)` | Needs review |
| `EnsureAgentWithParent` | `(name, workDir, model, parent string, autoStart bool) (*AgentDef, error)` | Needs review |
| `Get` | `(name string) *Agent` | Stable |
| `IsAncestor` | `(ancestor, name string) bool` — strict ancestry over `Parent` links, cycle-safe | Needs review |
| `Launch` | `(name string) (*Agent, error)` | Stable |
| `List` | `() []AgentDef` | Stable |
| `MarkMaterialized` | `(name string) error` — persists `Materialized=true` so later launches pass `RequireResume` | Needs review |
| `Register` | `(def AgentDef) error` | Stable |
| `Remove` | `(name string) error` | Stable |
| `StartAll` | `()` | Stable |
| `Stop` | `(name string)` | Stable |
| `StopAll` | `()` | Stable |

### Package `github.com/marcelocantos/claudia/codex`

New public package as of v0.21.0. It is the Codex Task backend in its own
right: a caller can drive `codex exec --json` through it directly, without
going via `claudia.Task` and `ProviderCodex`. The whole package is **Fluid**
— it exists to be exercised, and the duplication between `codex.Task` and
`claudia.Task` is unresolved (see § Gaps).

#### Types

| Item | Definition | Status |
|---|---|---|
| `AuthError` | struct with `Message string`; method `Error() string`. Subscription/auth preflight or JSONL auth failure | Fluid |
| `Config` | struct with `ID, Name, WorkDir, Model, SandboxMode, ApprovalPolicy, SessionID string`, `Resolve *ResolveArgs`, `RawLog func(line []byte)` | Fluid |
| `Event` | struct with `Type EventType`, `Content, ToolName, ToolInput, ToolID, SessionID string`, `Usage Usage`, `Error error` | Fluid |
| `EventType` | string type: `EventInit`, `EventText`, `EventToolUse`, `EventResult`, `EventError` | Fluid |
| `ExitError` | struct with `Message string`, `ExitCode int`; method `Error() string`. Non-zero exit with no prior structured error | Fluid |
| `RateLimitError` | struct with `Message string`; method `Error() string`. Throttle / 429 / usage cap | Fluid |
| `ResolveArgs` | struct with `BinPath, AuthPath string`, `Getenv func(string) string`, `LookPath func(string) (string, error)`, `Stat func(string) (os.FileInfo, error)`, `SkipAuthPreflight bool`. Test seam for binary resolution and auth preflight | Fluid |
| `RunError` | struct with `Message string`; method `Error() string`. Unclassified run failure | Fluid |
| `Status` | string type: `StatusIdle`, `StatusRunning`, `StatusError`, `StatusStopped` | Fluid |
| `Task` | opaque struct; methods listed below | Fluid |
| `Usage` | struct with `InputTokens, OutputTokens, CacheReadInputTokens int`. Note: no `CacheCreationInputTokens`, unlike `claudia.Usage` | Fluid |

#### Constants

| Item | Status |
|---|---|
| `EventInit, EventText, EventToolUse, EventResult, EventError` (EventType) | Fluid |
| `StatusIdle, StatusRunning, StatusError, StatusStopped` (Status) | Fluid |

#### Functions

| Item | Signature | Status |
|---|---|---|
| `ClassifyFailure` | `ClassifyFailure(msg string) error` — maps a free-text Codex failure onto `*AuthError` / `*RateLimitError`, else `*RunError` | Fluid |
| `NewCodexTask` | `NewCodexTask(cfg Config) *Task` — alias for `NewTask` | Fluid |
| `NewTask` | `NewTask(cfg Config) *Task` | Fluid |
| `ParseLines` | `ParseLines(lines [][]byte) []Event` — hermetic fixture-test convenience | Fluid |

#### `Task` methods

| Item | Signature | Status |
|---|---|---|
| `ID` | `() string` | Fluid |
| `LastResult` | `() string` | Fluid |
| `Name` | `() string` | Fluid |
| `Run` | `(ctx context.Context, prompt string) (<-chan Event, error)` | Fluid |
| `SessionID` | `() string` — the Codex thread id, the resume handle for a later `Run` | Fluid |
| `Status` | `() Status` | Fluid |
| `Stop` | `()` | Fluid |
| `WorkDir` | `() string` | Fluid |

### Package `github.com/marcelocantos/claudia/grok`

#### Types

| Item | Definition | Status |
|---|---|---|
| `Client` | opaque struct; methods listed below | Stable |
| `Config` | struct with `APIKey string`, callback fields (`OnAudio, OnTranscript, OnTranscriptDone, OnUserTranscript, OnFunctionCall, OnSessionReady, OnResponseDone, OnError`), `Voice string`, `Tools []Tool`, `SystemPrompt string`, `ManualCommit bool`, `Dial *DialArgs` | Needs review |
| `DialArgs` | struct with `URL string` and `Dial func(ctx, url, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)`. Test seam for reaching the Realtime endpoint; nil is valid. Leaks `nhooyr.io/websocket` into the public signature | Fluid |
| `ResponseModalities` | `[]string`; selects which output modalities Grok generates for a response | Needs review |
| `Tool` | struct with `Type, Name, Description string` and `Parameters json.RawMessage` | Stable |

#### Variables

| Item | Status |
|---|---|
| `ModalitiesTextAudio, ModalitiesText` (ResponseModalities) | Needs review |

#### Functions

| Item | Signature | Status |
|---|---|---|
| `Connect` | `Connect(ctx context.Context, cfg Config) (*Client, error)` | Stable |

#### `Client` methods

| Item | Signature | Status |
|---|---|---|
| `ClearBuffer` | `(ctx context.Context) error` | Needs review |
| `Close` | `() error` | Stable |
| `Commit` | `(ctx context.Context) error` | Needs review |
| `CommitAndRespond` | `(ctx context.Context) error` | Needs review |
| `InjectAssistantText` | `(ctx context.Context, text string) error` | Fluid — deprecated, prefer `SendSystemNote` |
| `InjectConversationItem` | `(ctx context.Context, role, text string) error` | Needs review |
| `RequestResponse` | `(ctx context.Context, modalities ResponseModalities) error` | Needs review |
| `SendAudio` | `(ctx context.Context, pcm []byte) error` | Stable |
| `SendSystemNote` | `(ctx context.Context, text string, modalities ResponseModalities) error` | Needs review |
| `SendText` | `(ctx context.Context, text string, modalities ResponseModalities) error` | Needs review |

### Environment variables

Environment variables are public surface: they change behaviour without a
code change, so a 1.0 contract has to name them. Only variables read by the
public packages are listed; `internal/` variables are not surface.

| Item | Purpose | Status |
|---|---|---|
| `AWS_DEFAULT_REGION` | Region fallback for `ProviderBedrock` when neither `TaskConfig` nor `CLAUDIA_BEDROCK_REGION` nor `AWS_REGION` names one. | Fluid |
| `XDG_STATE_HOME` | Root for claudia's own state — terminal logs (`<state>/claudia/terms/…`), the chain sidecar, and the Grok connect-mode record. Defaults to `~/.local/state`. | Needs review |
| `AWS_PROFILE` | Shared-config profile for the `ProviderBedrock` credential chain. | Fluid |
| `AWS_REGION` | Region for `ProviderBedrock`, below `CLAUDIA_BEDROCK_REGION`. | Fluid |
| `CLAUDE_BIN` | Absolute path or PATH-resolvable name of the `claude` executable. Honoured by both Task and Session/Pool spawn paths. Falls back to `exec.LookPath("claude")` then to known install locations (`~/.local/bin/claude`, `~/.claude/local/claude`, `/opt/homebrew/bin/claude`, `/usr/local/bin/claude`). | Stable |
| `CLAUDIA_BEDROCK_MODEL_ID` | Bedrock model id for `ProviderBedrock` when `TaskConfig.Model` is empty. | Fluid |
| `CLAUDIA_BEDROCK_REGION` | Highest-priority region for `ProviderBedrock`. | Fluid |
| `CLAUDIA_CLAUDE_OAUTH_TOKEN` | Overrides the keychain lookup for `QueryPlanUsage(ProviderClaude)`. Below `PlanUsageArgs.ClaudeAccessToken`. | Fluid |
| `CLAUDIA_CODEX_ACCESS_TOKEN` | Overrides the `auth.json` lookup for `QueryPlanUsage(ProviderCodex)`. Below `PlanUsageArgs.CodexAccessToken`. | Fluid |
| `CLAUDIA_CODEX_ACCOUNT_ID` | ChatGPT account id for the Codex plan-usage request. Below `PlanUsageArgs.CodexAccountID`. | Fluid |
| `CLAUDIA_CODEX_AUTH_PATH` | Overrides `~/.codex/auth.json` for `PreflightCodexAuth` and for `codex` binary resolution. | Fluid |
| `CLAUDIA_GROK_CONNECT` | Named by the exported const `EnvGrokConnect`. Truthy (`1`, `true`, `yes`, `on`) forces Grok connect-mode on Session `Start` even when `Config.GrokConnect` is false. | Fluid |
| `CLAUDIA_GROK_USAGE` | Opts into the undocumented Grok billing endpoint behind `QueryPlanUsage(ProviderGrok)`. Off by default because the surface is private and unversioned. | Fluid |
| `CODEX_BIN` | Absolute path or PATH-resolvable name of the `codex` executable. Honoured by Codex Task mode. Falls back to `exec.LookPath("codex")` then to known install locations including `/Applications/ChatGPT.app/Contents/Resources/codex`. | Fluid |
| `GROK_BIN` | Absolute path or PATH-resolvable name of the Grok Build CLI (`grok`). Honoured by Grok Task mode. Falls back to `exec.LookPath("grok")` then to known install locations including `~/.grok/bin/grok`. Not related to package `claudia/grok` (Realtime voice). | Fluid |
| `OPENAI_API_KEY` | Read only for *detection* by `PreflightCodexAuth`: when set, the ChatGPT-subscription assertion fails (`SubscriptionOK=false`) because Codex would bill per token. claudia never sets or forwards it. | Fluid |

`CLAUDIA_LIVE`, `CLAUDIA_BEDROCK_LIVE`, `CLAUDIA_CODEX_LIVE` and
`CLAUDIA_GROK_LIVE` gate live tests. They are test-harness switches, not
runtime surface, and carry no compatibility commitment.

### Provider capability matrix

Every gap is published rather than discovered at runtime. Query it with
`ProviderCapabilityMatrix(provider)`, or gate a call ahead of time with
`CheckCapability`, which returns the same `*CapabilityError` the operation
itself would have returned. The table below is the state at v0.21.0; the
claim table in `capability.go` is the source of truth, and
`TestProviderCapabilityMatrixIsTotal` fails if a provider goes silent on a
capability rather than claiming one — silence must never read as support.

| Capability | Claude | Codex | Grok | Bedrock |
|---|---|---|---|---|
| `CapabilityTask` | Supported | Supported | Supported | Supported |
| `CapabilitySession` | Supported | **Experimental** | Supported | Unsupported |
| `CapabilityResume` | Supported | Supported | Supported | Unsupported |
| `CapabilityRewind` | Supported | Unsupported | Unsupported | Unsupported |
| `CapabilityCost` | Supported | Unsupported | Unsupported | Unsupported |
| `CapabilityTmuxAttach` | Supported | Unsupported | Unsupported | Unsupported |
| `CapabilityTerminalLog` | Supported | Unsupported | Unsupported | Unsupported |
| `CapabilityPermissionMode` | Supported | Unsupported | Unsupported | Unsupported |
| `CapabilityToolRestrictions` | Supported | Unsupported | Unsupported | Unsupported |
| `CapabilityImageInput` | Unsupported | Unsupported | Unsupported | Unsupported |
| `CapabilityWebSearch` | Supported | Unsupported | Unsupported | Unsupported |

`CapabilityImageInput` is unsupported everywhere for one reason: claudia has
no API for attaching an image to a prompt on any provider. That is a claudia
gap, not four provider gaps.

### Bedrock provider surface

`ProviderBedrock` reaches Anthropic Claude models through the AWS Bedrock
`ConverseStream` API. There is no local CLI and no local process, which is
why almost every capability outside `CapabilityTask` is unsupported: v1 is
stateless, so there is no provider-side session to resume, no transcript to
rewind, no tmux window to attach, and no PTY to log. `ConverseStream`
reports token counts, and claudia does not price them, so `CostUSD` stays
zero. claudia does not expose Bedrock tool configuration, so
`TaskConfig.DisallowTools` cannot be honoured and is refused rather than
dropped.

Region and model resolve from `TaskConfig` first, then
`CLAUDIA_BEDROCK_REGION` / `CLAUDIA_BEDROCK_MODEL_ID`, then the AWS
variables. Credential material stays in the AWS SDK default chain —
claudia reads no keys and holds none.

### Codex provider surface

Codex support is pre-1.0 and intentionally capability-gated. The stable
surface today is Task mode through `codex exec --json`, selected by
`TaskConfig.Provider = ProviderCodex`. Token usage maps into `Usage`,
but Codex Task mode does not currently report cost in `CostUSD`.

Persistent Codex Session mode is not implemented yet. `Start` with
`Config.Provider = ProviderCodex` fails closed with `*CapabilityError`
and `Status == CapabilityExperimental` until the public app-server
thread/turn contract is proven. Codex rewind, tmux attach, terminal
byte logs, and Claude-style transcript manipulation are unsupported;
callers should expect `CapabilityError` rather than silent Claude
fallback semantics.

The per-capability rationale claudia publishes for Codex:

| Capability | Codex | Rationale |
|---|---|---|
| `CapabilityTask` | Supported | `codex exec --json` |
| `CapabilityResume` | Supported | `codex exec resume --json` |
| `CapabilitySession` | **Experimental** | app-server live contract not yet wired into production `Start` |
| `CapabilityRewind` | Unsupported | needs a public fork/resume contract; private transcript truncation is forbidden |
| `CapabilityCost` | Unsupported | tokens are reported, monetary cost is not; `CostUSD` stays zero |
| `CapabilityTmuxAttach` | Unsupported | claudia does not drive the Codex TUI in tmux |
| `CapabilityTerminalLog` | Unsupported | Task mode consumes JSON, not a PTY |
| `CapabilityPermissionMode` | Unsupported | Codex sandbox/approval flags are Codex-native, not a Claude `PermissionMode` mapping |
| `CapabilityToolRestrictions` | Unsupported | `codex exec` has no per-tool disallow flag |
| `CapabilityImageInput` | Unsupported | claudia has no image-attachment API on any provider |
| `CapabilityWebSearch` | Unsupported | claudia does not bind Codex's `--search`; the Codex default applies |

Billing is asserted, not assumed. `PreflightCodexAuth` reads
`~/.codex/auth.json` (or `CLAUDIA_CODEX_AUTH_PATH`) without touching the
network and reports whether ChatGPT-subscription OAuth is the active path.
`SubscriptionOK` is false when an access token is missing *or* when
`OPENAI_API_KEY` is set in the environment, because either way the run may
bill per token. Callers that must not bill per token should refuse to spawn
rather than proceed hopefully.

Codex sandbox and approval fields are passed to Codex as Codex flags.
They are not treated as equivalent to Claude `PermissionMode` or
`DisallowTools` until tests prove a narrower mapping.

Consequently `Task.Run` **refuses** a Codex task that carries
`TaskConfig.DisallowTools`, returning `*CapabilityError` with
`Capability == CapabilityToolRestrictions` before any process is
spawned. Running it would hand the caller a fully-armed agent while
they believe the tools were removed. `BaseDisallowedTools` names Claude
Code tools (`Agent`, `TeamCreate`, …) that do not exist in `codex
exec`, so it is vacuous there and does not by itself block a task.

### Grok Build CLI provider surface

Grok Build CLI support is pre-1.0 and capability-gated.

**Task mode** uses `grok -p … --output-format streaming-json`
(`TaskConfig.Provider = ProviderGrok`). Session id is taken from the
terminal `end.sessionId` field; headless streaming-json does not map
tool_use into `TaskEvent`. Cost is not reported in `CostUSD`.

**Session mode** uses ACP over `grok agent --always-approve stdio`
(`Config.Provider = ProviderGrok`). `Send` / `WaitForResponse` /
`Interrupt` / `Stop` are supported. There is no tmux attach or terminal
byte log. Rewind via private session files is unsupported
(`CapabilityUnsupported`). Resume uses ACP `session/load` when
`Config.SessionID` is set. `RequireResume` fail-closes on load
failure and never mints a replacement id, including when MCP is
configured. `MCPConfig` does not skip load. Without `RequireResume`,
an unmaterialized id may fall through to `session/new`.

**Permission mode and tool restrictions are both unsupported, and
`Task.Run` refuses rather than drops.** Task mode hardcodes
`--permission-mode bypassPermissions`; `Config.PermissionMode` is not
mapped onto it. A Grok task carrying `TaskConfig.DisallowTools` is
refused before any process is spawned, returning `*CapabilityError`
with `Capability == CapabilityToolRestrictions`.

Unlike `codex exec`, the refusal is not for want of a mechanism:
`grok --deny <RULE>` gates tool invocations and `grok
--disallowed-tools <IDS>` removes built-in tools from the toolset.
claudia translates `DisallowTools` into neither. The hardcoded
`bypassPermissions` resolves to a catch-all allow rule, and `grok`
accepts unrecognised tool names silently, so an untested translation
would reinstate the silent drop under a `CapabilitySupported` claim.
Until the translation is wired and proven, the claim stays
`CapabilityUnsupported` and the run is refused.

`BaseDisallowedTools` is applied on Claude only. It is never passed to
Grok, so nothing removes `Agent`, `TeamCreate` and friends from a Grok
agent — those names are Claude Code tools that Grok does not implement
in the first place.

Do not confuse `ProviderGrok` with package
`github.com/marcelocantos/claudia/grok`, which is a standalone Realtime
voice WebSocket client.

### Surface item count

192 top-level items at v0.21.0 — 138 in `claudia`, 36 in `claudia/codex`,
18 in `claudia/grok` — counting types, functions, methods, constants and
variables, but not struct fields. With fields, 366. The comparable count
at v0.17.0, the previous snapshot, was 100 across two packages: the
surface nearly doubled in one release, almost all of it additive.

Per the release skill's pre-1.0 → 1.0 shakeout gate (B.3a), the minimum
settling period is **1 month** with no backwards-incompatible changes
since the last breaking release. Historical SemVer practice scaled this by
surface size (3+ months for >50 items); the LLM-coding era compresses
real-world API exercise enough that a flat 1-month minimum suffices.

The clock resets if a breaking change ships mid-shakeout. Diffing the
derived surface at every tag from v0.12.0 to v0.21.0 finds exactly one
reset after v0.12.0's `grok.Client.SendText` signature change: **v0.21.0
retyped `CapabilityError.Capability` and `CapabilityError.Status` from
`string` to the new `Capability` and `CapabilityStatus` types.** That is
source-incompatible for any caller that assigned either field to a
`string` variable or passed it where a `string` was expected. Every other
tag in that range removed or changed nothing. v0.21.0 was tagged
2026-08-10, so the earliest eligible 1.0 cut date is **2026-09-10**.

## Gaps and prerequisites for 1.0

Concrete items that must be addressed before cutting 1.0.

### API design questions raised by the v0.21.0 surface

Raised by this snapshot; none are resolved.

- **`codex.Task` duplicates `claudia.Task`.** The `codex` package
  publishes its own `Task`, `Config`, `Event`, `EventType`, `Status` and
  `Usage`, parallel to the root package's, and a caller can drive Codex
  through either. Two entry points for one provider is a 1.0 liability:
  either the subpackage is the implementation detail behind
  `ProviderCodex` and should not be public, or it is the supported
  Codex API and the root package's Codex path is the wrapper. Note that
  `codex.Usage` has no `CacheCreationInputTokens` while `claudia.Usage`
  does, so the two are not interchangeable today.
- **`grok.DialArgs` leaks `nhooyr.io/websocket` into the public
  signature.** `DialArgs.Dial` names `*websocket.DialOptions` and
  `*websocket.Conn`, so the module cannot change WebSocket library
  without a breaking change, for the sake of a test seam.
- **The plan-usage API is shaped by private endpoints.** `PlanUsageArgs`
  and `AllPlanUsageArgs` carry per-provider credential and URL overrides
  (nine string fields plus `GrokBillingRaw`), and the Grok path reads an
  undocumented, unversioned billing endpoint gated behind
  `CLAUDIA_GROK_USAGE`. The struct will churn as those endpoints move.
  It is the largest Fluid block in the surface.
- **`CodexAuthPreflight` is Codex-specific surface on the root package.**
  Eight fields naming Codex `auth.json` internals sit beside the
  provider-neutral API. If a second provider needs a billing-mode
  assertion, this wants to be a general shape rather than a second
  parallel struct.
- **`PurposeWork` / `PurposeAside` / `PurposeOverseer` are untyped
  string constants**, unlike every other constant group in the package.
  `AgentDef.Purpose` is a bare `string`, so the constants do not
  constrain it. A `Purpose` named type would make it checkable.

### ~~API design fixes (breaking)~~

Resolved in v0.10.0. The following renames and type changes shipped
together as a single breaking release:

- ~~**Rename `Task` accessor methods.** `TaskID`, `TaskName`,
  `TaskWorkDir`, `TaskStatus` should be `ID`, `Name`, `WorkDir`,
  `Status`.~~
- ~~**Remove `Task.SetLastResult`.**~~ Removed; restoration is now
  via `TaskConfig.LastResult` at construction time.
- ~~**Rename `Task.CancelTask` / `Task.StopTask`** to `Cancel` /
  `Stop`.~~
- ~~**Rename `Task.RunTask`** to `Run`.~~
- ~~**Audit `AgentDef.Parent`.**~~ Removed (no consumer used it).
- ~~**`Config.DisallowTools` is comma-separated.**~~ Now `[]string`.
  `AgentDef.DisallowTools` follows suit; the persisted JSON shape
  changes from a comma-separated string to an array.
- ~~**`Event.Raw` type mismatch.**~~ Now declared as `[]byte` and
  populated explicitly.
- ~~**`Registry.Start` shadows package-level `Start`.**~~ Renamed
  to `Launch`.

### ~~Behavioural fixes~~

- ~~**`Stop` has a hard 1-second sleep.**~~ Resolved by tmux pivot:
  `Stop` now calls `tmux kill-window` which terminates immediately.
- ~~**`TermLogPath` lies after write failures.**~~ Fixed: `TermLogPath()`
  now returns `""` once the log file has been closed due to a write
  error. See `agents-guide.md` § Gotcha 4.
- ~~**Terminal log lacks run-boundary markers.**~~ Resolved by deliberate
  design decision: the `.term` file is a raw PTY rendering aid, not a
  structured transcript. No markers will be added. Documented in
  `agents-guide.md` § Gotcha 4.
- ~~**Session mode has no cost or usage accounting.**~~ Fixed: `Agent.Usage()`
  returns cumulative token counts parsed from the JSONL transcript.
  Documented in `agents-guide.md` § Usage accounting.
- ~~**`OnEvent` is a single handler.**~~ Fixed: replaced with
  `SubscribeEvents(fn EventFunc) int64` / `UnsubscribeEvents(token int64)`,
  matching the `SubscribeTerminal`/`UnsubscribeTerminal` pattern.
  Multiple subscribers are supported; `WaitForResponse` uses this
  internally without displacing external subscribers.
- ~~**Readiness tuning is hardcoded.**~~ Resolved by deliberate design
  decision: 50 ms poll, 30 s cap, empirically-observed ~680 ms startup
  on macOS. No config surface will be added speculatively. Documented
  in `agents-guide.md` § Readiness detection.

### ~~Documentation~~

- ~~**Package doc comments are thin.** `claudia` and `grok` have
  top-level package comments but type-level docs are inconsistent.
  `go doc` output should be self-sufficient before 1.0.~~
- ~~**No examples in `_test.go` files.** Add `Example` functions so
  pkg.go.dev renders runnable snippets.~~

Resolved in v0.11.0 — all exported types, functions, methods, and constants
have doc comments, and `example_test.go` adds `ExampleRun`, `ExampleNewTask`,
`ExampleStart`, `ExampleAcquire`, and `ExampleNewRegistry`.

### Testing and CI

- ~~**No CI workflow.**~~ Resolved: `.github/workflows/test.yml`
  landed in PR #5 and runs on push.
- **Test coverage is growing.** Agent readiness, crash-survival,
  WaitForResponse settle semantics, event parsing, and terminal-log
  path derivation are covered. ~~Task mode still has no end-to-end
  smoke test against a real `claude` binary.~~ Resolved: `TestTaskRunSmoke`
  in `task_test.go` covers Task-mode end-to-end against the real binary
  (gated on `CLAUDIA_LIVE=1`).
- ~~**CI does not exercise tmux-backed Agent** on Linux runners.
  GitHub macOS runners have tmux pre-installed; Linux runners need
  `apt-get install tmux`. See 🎯T1.1 M6.~~ Resolved by deliberate scope
  decision: CI installs tmux on Linux and runs all hermetic tests on
  both macOS and Linux. Live tests (those that spawn the real `claude`
  binary and make API calls) are gated on `CLAUDIA_LIVE=1` and run
  locally before each release. See `agents-guide.md` § Testing for the
  canonical pre-release validation command.

### Packaging

- ~~**No version constant.**~~ Resolved in v0.10.0 — `claudia.Version` exposes the build's released version.

## Out of scope for 1.0

- **Replacing the `claude` CLI shell-out with a native API.** Not
  happening — claudia exists specifically because there is no such
  API. If Anthropic ships one, it becomes a separate project.
- **Arbitrary multi-backend support (OpenAI Chat Completions, Gemini,
  etc. as coding agents).** claudia harnesses terminal coding-agent
  CLIs (Claude Code, Codex, Grok Build). The `grok` subpackage covers
  Realtime voice only and is not a generic LLM SDK.
- **WebSocket / HTTP server wrapping.** The concern of the host
  program, not this library.
- **Windows support for the tmux-backed Agent.** The tmux substrate
  is Unix-only. Windows consumers who want the Agent must use WSL.
  This is a deliberate tradeoff for the crash-survival and
  observability that tmux provides. The `RegisterChain` /
  `LookupChain` sidecar machinery is cross-platform as of v0.7.0 —
  Windows consumers that only need chain tracking do not require
  WSL.
- **Built-in persistence for Task sessions.** The `Registry` handles
  session mode agents; Task consumers can persist their own state.

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

Snapshot as of: v0.6.0.

## Interaction surface

The exhaustive list of public-facing items in the module. Each item
is annotated with a stability assessment:

- **Stable** — unlikely to change. Design is settled.
- **Needs review** — functional but may benefit from refinement
  before locking in.
- **Fluid** — actively evolving or known to need rework.

### Package `github.com/marcelocantos/claudia`

#### Types

| Item | Definition | Status |
|---|---|---|
| `Config` | struct with `WorkDir, SessionID, Model, PermissionMode, MCPConfig, DisallowTools, TermLogPath, PoolPolicy string`, `ExtraArgs []string`, `PoolCap int` | Needs review |
| `Agent` | opaque struct; methods listed below | Needs review |
| `Event` | struct with `Type string`, `Raw json.RawMessage`, `Text string`, `StopReason string`, `ProgressType string`; method `IsTerminalStop() bool` | Stable |
| `EventFunc` | `func(Event)` | Needs review |
| `Usage` | struct with `InputTokens, OutputTokens, CacheCreationInputTokens, CacheReadInputTokens int` | Stable |
| `TaskEvent` | struct with `Type TaskEventType`, `Content, ToolName, ToolInput, ToolID, SessionID string`, `DurationMs, CostUSD float64`, `Usage Usage`, `IsError bool`, `ErrorMsg string` | Needs review |
| `TaskEventType` | string type | Stable |
| `TaskStatus` | string type | Stable |
| `TaskConfig` | struct with `ID, Name, WorkDir, Model, ClaudeID string` | Needs review |
| `RawLogFunc` | `func(line []byte)` | Stable |
| `Task` | opaque struct; methods listed below | Fluid |
| `AgentDef` | struct with `Name, WorkDir, SessionID, Model, Parent, DisallowTools string` and `AutoStart bool` | Fluid |
| `Registry` | opaque struct; methods listed below | Needs review |

#### Constants

| Item | Status |
|---|---|
| `TaskEventInit, TaskEventText, TaskEventToolUse, TaskEventResult, TaskEventError` (TaskEventType) | Stable |
| `TaskStatusIdle, TaskStatusRunning, TaskStatusError, TaskStatusStopped` (TaskStatus) | Stable |
| ~~`ErrDaemonUnavailable`~~ | Removed (daemon pivot) |

#### Functions

| Item | Signature | Status |
|---|---|---|
| `Start` | `Start(cfg Config) (*Agent, error)` | Needs review |
| `Run` | `Run(ctx context.Context, prompt string, cfg Config) (string, error)` | Stable |
| `NewTask` | `NewTask(cfg TaskConfig) *Task` | Stable |
| `ParseTaskLine` | `ParseTaskLine(line []byte) []TaskEvent` | Stable |
| `NewRegistry` | `NewRegistry(path string) (*Registry, error)` | Stable |
| `Acquire` | `Acquire(ctx context.Context, cfg Config) (*Agent, error)` | Needs review |
| `RegisterChain` | `RegisterChain(chainID, sessionID string) error` | Needs review |
| `LookupChain` | `LookupChain(sessionID string) (string, []string, error)` | Needs review |

#### `Agent` methods

| Item | Signature | Status |
|---|---|---|
| `SessionID` | `() string` | Stable |
| `JSONLPath` | `() string` | Stable |
| `TermLogPath` | `() string` | Needs review |
| `Alive` | `() bool` | Stable |
| `WaitReady` | `(ctx context.Context) error` | Stable |
| `OnEvent` | `(fn EventFunc)` | Fluid |
| `Interrupt` | `() error` | Stable |
| `Send` | `(msg string) error` | Stable |
| `WaitForResponse` | `(ctx context.Context) (string, error)` | Needs review |
| `Resize` | `(cols, rows uint16) error` | Stable |
| `Stop` | `()` | Needs review |
| `Release` | `(disposition string) error` | Needs review |
| `AttachCommand` | `() string` | Needs review |
| `SubscribeTerminal` | `() (history []byte, ch chan []byte)` | Needs review |
| `UnsubscribeTerminal` | `(ch chan []byte)` | Needs review |

#### `Task` methods

| Item | Signature | Status |
|---|---|---|
| `TaskID` | `() string` | Fluid |
| `TaskName` | `() string` | Fluid |
| `TaskWorkDir` | `() string` | Fluid |
| `TaskStatus` | `() TaskStatus` | Fluid |
| `LastResult` | `() string` | Needs review |
| `ClaudeID` | `() string` | Stable |
| `SetRawLog` | `(fn RawLogFunc)` | Stable |
| `SetLastResult` | `(r string)` | Fluid |
| `RunTask` | `(ctx context.Context, prompt string) (<-chan TaskEvent, error)` | Fluid |
| `CancelTask` | `() error` | Fluid |
| `StopTask` | `()` | Fluid |

#### `Registry` methods

| Item | Signature | Status |
|---|---|---|
| `Register` | `(def AgentDef) error` | Stable |
| `Remove` | `(name string) error` | Stable |
| `Start` | `(name string) (*Agent, error)` | Needs review |
| `Stop` | `(name string)` | Stable |
| `Get` | `(name string) *Agent` | Stable |
| `Def` | `(name string) *AgentDef` | Stable |
| `List` | `() []AgentDef` | Stable |
| `StartAll` | `()` | Stable |
| `StopAll` | `()` | Stable |
| `EnsureAgent` | `(name, workDir, model string, autoStart bool) (*AgentDef, error)` | Needs review |

### Package `github.com/marcelocantos/claudia/grok`

#### Types

| Item | Definition | Status |
|---|---|---|
| `Tool` | struct with `Type, Name, Description string` and `Parameters json.RawMessage` | Stable |
| `Config` | struct with `APIKey string`, callback fields (`OnAudio, OnTranscript, OnTranscriptDone, OnUserTranscript, OnFunctionCall, OnSessionReady, OnError`), `Voice string`, `Tools []Tool`, `SystemPrompt string` | Needs review |
| `Client` | opaque struct; methods listed below | Stable |

#### Functions

| Item | Signature | Status |
|---|---|---|
| `Connect` | `Connect(ctx context.Context, cfg Config) (*Client, error)` | Stable |

#### `Client` methods

| Item | Signature | Status |
|---|---|---|
| `SendAudio` | `(ctx context.Context, pcm []byte) error` | Stable |
| `SendText` | `(ctx context.Context, text string) error` | Stable |
| `InjectAssistantText` | `(ctx context.Context, text string) error` | Needs review |
| `Close` | `() error` | Stable |

### Surface item count

~60 items across both packages. Per the release skill's settling
table, this puts claudia in the 50–100 bracket with a minimum
settling period of 3 months from the last breaking change.

## Gaps and prerequisites for 1.0

Concrete items that must be addressed before cutting 1.0.

### API design fixes (breaking)

- **Rename `Task` accessor methods.** `TaskID`, `TaskName`,
  `TaskWorkDir`, `TaskStatus` should be `ID`, `Name`, `WorkDir`,
  `Status`. The current names repeat the receiver type.
- **Remove `Task.SetLastResult`.** It exists only as a "restore from
  DB" hack exposed as public API. Either move the restore path to a
  constructor option or make it package-private.
- **Rename `Task.CancelTask` / `Task.StopTask`** to `Cancel` / `Stop`
  for consistency with `Agent.Stop`.
- **Rename `Task.RunTask`** to `Run`, or accept the package-level
  naming collision and document it.
- **Audit `AgentDef.Parent`.** Described as "for tree display" but
  unused by the library. Remove if no consumer needs it.
- **`Config.DisallowTools` is comma-separated.** Should be
  `[]string` for parity with `ExtraArgs`.
- **`Event.Raw` type mismatch.** Declared as `json.RawMessage` but
  populated via `json.RawMessage(line)` where `line` is a `string`.
  This works by coincidence; should be `[]byte` from the start or
  the parser should convert explicitly.
- **`Registry.Start` shadows package-level `Start`.** Confusing at
  call sites; consider renaming to `Launch` or `StartAgent`.

### Behavioural fixes

- ~~**`Stop` has a hard 1-second sleep.**~~ Resolved by tmux pivot:
  `Stop` now calls `tmux kill-window` which terminates immediately.
- **`TermLogPath` lies after write failures.** The accessor returns
  the configured path even after `pushTermOutput` silently disabled
  the log on write error. Either keep logging best-effort with an
  accessor for "is logging live" or return "" once disabled.
- **Terminal log lacks run-boundary markers.** Resumed sessions
  concatenate terminal output with no way to split runs. Decide on a
  marker format (or don't, and document the choice) before 1.0.
- **Session mode has no cost or usage accounting.** Only Task mode
  exposes this. Either document the asymmetry or parse usage from
  the JSONL transcript.
- **`OnEvent` is a single handler.** Replace with either a
  subscribe/unsubscribe pattern (like `SubscribeTerminal`) or a
  channel-returning primitive so multiple consumers can observe
  events without colliding.
- **Readiness tuning is hardcoded.** `detectReady` uses a capture-pane
  regex polled at 50 ms with a 30 s overall cap. The values are
  reasonable (startup readiness observed at ~680 ms on macOS) but
  not configurable via `Config`. Expose if consumers report timeouts.

### Documentation

- **Package doc comments are thin.** `claudia` and `grok` have
  top-level package comments but type-level docs are inconsistent.
  `go doc` output should be self-sufficient before 1.0.
- **No examples in `_test.go` files.** Add `Example` functions so
  pkg.go.dev renders runnable snippets.

### Testing and CI

- ~~**No CI workflow.**~~ Resolved: `.github/workflows/test.yml`
  landed in PR #5 and runs on push.
- **Test coverage is growing.** Agent readiness, crash-survival,
  WaitForResponse settle semantics, event parsing, and terminal-log
  path derivation are covered. Task mode still has no end-to-end
  smoke test against a real `claude` binary.
- **CI does not exercise tmux-backed Agent** on Linux runners.
  GitHub macOS runners have tmux pre-installed; Linux runners need
  `apt-get install tmux`. See 🎯T1.1 M6.

### Packaging

- **No version constant.** claudia has no in-source version string.
  Consumers rely on `go.mod` pinning. Consider adding a
  `const Version = "x.y.z"` that the release skill keeps in sync, so
  runtime diagnostics can report the library version.

## Out of scope for 1.0

- **Replacing the `claude` CLI shell-out with a native API.** Not
  happening — claudia exists specifically because there is no such
  API. If Anthropic ships one, it becomes a separate project.
- **Multi-backend support (OpenAI, Gemini, etc. as coding agents).**
  claudia is Claude Code specific by definition. The `grok`
  subpackage covers voice only and does not make claudia a
  multi-backend library.
- **WebSocket / HTTP server wrapping.** The concern of the host
  program, not this library.
- **Windows support.** The tmux substrate is Unix-only. Windows
  consumers must use WSL. This is a deliberate tradeoff for the
  crash-survival and observability that tmux provides.
- **Built-in persistence for Task sessions.** The `Registry` handles
  session mode agents; Task consumers can persist their own state.

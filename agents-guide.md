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
| Cost accounting     | Yes, per prompt                      | No (transcript only)                    |
| Resume across runs  | Via `TaskConfig.ClaudeID`            | Via `Config.SessionID`                  |

**Default to Task mode.** It's simpler, gives you structured events,
and exposes cost and token accounting. Only use Session mode if the
user explicitly needs persistent state or wants to observe the
transcript live.

## Task mode: essential patterns

Construct with `NewTask`, then call `RunTask` to get a channel of
events:

```go
task := claudia.NewTask(claudia.TaskConfig{
    ID:      "unique-id",
    WorkDir: "/abs/path",
    Model:   "sonnet", // or "opus", or "" for default
})
events, err := task.RunTask(ctx, prompt)
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

**Cancellation**: `Task.CancelTask()` sends SIGINT to the running
process; `Task.StopTask()` cancels and marks the task as stopped so
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

Set an event handler **before** sending the first message — messages
may arrive quickly, and only one handler is active at a time:

```go
agent.OnEvent(func(ev claudia.Event) {
    // ev.Type: "assistant", "user", "system", "progress", ...
    // ev.Text: concatenated text for assistant turns
    // ev.Raw:  complete JSONL line
})

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

## Registry (optional)

`claudia.Registry` persists agent definitions to a JSON file and
manages their processes. Useful when the host program needs to:

- Auto-start several agents on boot
- Resume agents by name across program restarts
- Rename or reassign agents without losing session history

Construct with `NewRegistry(path)`, then `Register` / `EnsureAgent`
to add definitions and `Start` / `StartAll` to launch them. If the
host program owns a single short-lived agent, skip the Registry.

Note: `Registry.Start(name)` shadows the package-level
`claudia.Start(cfg)`. They return the same type but take different
arguments.

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
   not supported; use WSL. Task mode does not require tmux.

2. **Sub-agents are disabled.** claudia always passes
   `--disallowedTools Agent,TeamCreate,TeamDelete,SendMessage,EnterWorktree`.
   The host Go program owns the process lifecycle; nested claudia
   sessions would fight over PTY ownership and transcript tailing.
   Don't try to re-enable these.

3. **Session resumption is automatic.** `Start` checks whether
   `<SessionID>.jsonl` exists under Claude Code's project directory.
   If it does, claudia passes `--resume`; otherwise `--session-id`.
   Pass a stable `SessionID` to get resumption for free.

4. **Terminal log files are append-only.** Resumed sessions
   accumulate PTY output across runs with no run-boundary markers.
   Don't treat the file as a single-session transcript without
   parsing it yourself.

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

8. **Task method names are verbose.** `TaskID()`, `TaskName()`,
   `TaskWorkDir()`, `TaskStatus()` repeat "Task" even though they
   are methods on `Task`. This will likely be renamed before 1.0 —
   see `STABILITY.md`.

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
`RunTask` invocation and relay results via `InjectAssistantText`.
Otherwise, ignore it.

## Stability

claudia is pre-1.0. `STABILITY.md` in the repo root tracks the public
interaction surface and flags which parts are stable, under review,
or still fluid. Consult it before building consumers that assume long
term API stability.

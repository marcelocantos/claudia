# claudia

Go library for embedding [Claude Code](https://claude.com/claude-code)
agents in any program.

claudia wraps the `claude` CLI in two complementary modes, so you can
drive Claude Code from a Go process without re-implementing PTY
handling, JSONL transcript tailing, or session lifecycle management.

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

events, err := task.RunTask(ctx, "Summarise the public API of this module.")
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

### Session mode — persistent conversations

Spawns `claude` inside a PTY and keeps it alive. Use it for multi-turn
conversations, interactive agents that respond to external events, or
programs that need to observe the session transcript as it happens.

```go
agent, err := claudia.Start(claudia.Config{
    WorkDir: "/path/to/repo",
    Model:   "opus",
})
if err != nil {
    log.Fatal(err)
}
defer agent.Stop()

agent.OnEvent(func(ev claudia.Event) {
    if ev.Type == "assistant" {
        fmt.Println(ev.Text)
    }
})

if err := agent.Send("What does this repo do?"); err != nil {
    log.Fatal(err)
}
reply, err := agent.WaitForResponse(ctx)
```

The one-shot helper `claudia.Run(ctx, prompt, cfg)` bundles `Start` +
`Send` + `WaitForResponse` + `Stop` for session mode if you want a
single call.

Resuming works automatically: if `Config.SessionID` is set and a JSONL
transcript already exists for it, claudia passes `--resume`; otherwise
it passes `--session-id` to create a fresh session with that ID.

The PTY output is also captured to
`$XDG_STATE_HOME/claudia/terms/<escaped-workdir>/<sessionID>.term`
(defaulting to `~/.local/state/...`) so you have a faithful record of
the rendered terminal view alongside the structured JSONL transcript.
Override via `Config.TermLogPath`; set to `"-"` to disable.

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

Requires:

- Go 1.26 or later
- The `claude` CLI installed and on `$PATH`

## For agents

If you use an agentic coding tool, include
[`agents-guide.md`](agents-guide.md) in your project context — it
covers the API surface, common patterns, and gotchas in a form
designed for LLM consumption.

The public API surface and its stability are tracked in
[`STABILITY.md`](STABILITY.md). claudia is pre-1.0; breaking changes
are possible until 1.0 locks in a backwards-compatibility contract.

## Licence

Apache 2.0 — see [LICENSE](LICENSE).

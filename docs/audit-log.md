# Audit Log

Chronological record of audits, releases, documentation passes, and other
maintenance activities. Append-only — newest entries at the bottom.

## 2026-04-12 — /release v0.4.0

- **Commit**: `12a5804`
- **Outcome**: Released v0.4.0 as a Go module. Fixed `WaitForResponse` hang
  on systems without stop hooks by switching from `system` events to terminal
  `stop_reason` on assistant events. Added `Agent.WaitReady` primitive and
  gated `Send` on PTY quiescence so consumers no longer race against TUI
  startup. Brought Session mode to parity with Task mode by stripping
  `CLAUDECODE` from the spawned child environment. Added test coverage for
  `parseEvent`, `IsTerminalStop`, path helpers, the Registry round-trip, and
  two live smoke tests exercising the readiness detector against a real
  `claude` binary. Measured readiness latency: 1.163 s end-to-end on an
  M4 Max / macOS 26.
- **Deferred**:
  - Readiness tuning constants (500 ms quiescence, 30 s cap) are hardcoded
    in `agent.go` — expose via `Config` if consumers hit slow-startup
    environments
  - `Task` method renames (drop the `Task` prefix on accessors, rename
    `RunTask`/`CancelTask`/`StopTask`) — tracked in `STABILITY.md` as 1.0
    prerequisites
  - `OnEvent` single-handler limitation — tracked in `STABILITY.md`
  - No CI workflow — tracked in `STABILITY.md` under Testing and CI

## 2026-04-12 — /release v0.5.0

- **Commit**: `1ffc493`
- **Outcome**: Released v0.5.0, a bug-fix release correcting three independent
  issues in v0.4.0's Send-to-response path that together caused consumers to
  hang on the first prompt. (1) `Send` now uses `\r` (Enter) instead of
  `\n` (Shift+Enter) as the submit key — the old key typed prompts into the
  input box without submitting them. (2) `Start` now resolves symlinks on the
  workdir via `filepath.EvalSymlinks`, fixing a macOS-specific path mismatch
  where claudia watched `-var-folders-...` while Claude Code wrote to
  `-private-var-folders-...`. (3) `WaitForResponse` now uses a 250 ms settle
  window to accumulate multi-block assistant messages instead of resolving on
  the first terminal `stop_reason`, fixing a bug where the thinking block
  (empty text, end_turn stop_reason) pre-empted the subsequent text block.
  Added four live e2e tests (`TestAgentSendAndWaitForResponse`,
  `TestAgentMultiTurn`, `TestRunHelper`, `TestTaskRunSmoke`) and five
  settle-timer unit tests. The v0.4.0 smoke test only exercised `WaitReady`
  in isolation and missed the entire Send-to-response flow, which is how
  those three bugs shipped; the new suite covers the user-facing flow.
- **Deferred**:
  - Readiness tuning constants still hardcoded — same note as v0.4.0
  - `Task` method renames — tracked in `STABILITY.md`
  - Session resume e2e test — worth adding in v0.6.0; the code path is
    exercised by the resumption branch in `Start` but has no direct test
  - `OnEvent` single-handler limitation — tracked in `STABILITY.md`
  - No CI workflow — tracked in `STABILITY.md`

## 2026-04-12 — /release v0.6.0

- **Commit**: `aa00c77`
- **Outcome**: Released v0.6.0, the tmux pivot release. Replaced the
  PTY-backed Agent and the claudiad daemon (PR #5, never released) with
  a tmux-backed substrate: agents run inside windows on a dedicated
  claudia tmux server (~/.local/state/claudia/tmux.sock). Added warm
  agent pool (Acquire/Release), filesystem-backed session-chain tracking
  (RegisterChain/LookupChain), tmux pre-flight check, and AttachCommand
  for human observability. Deleted ~2,200 lines of daemon/PTY code,
  dropped creack/pty, fsnotify, golang.org/x/sys dependencies. CI
  updated to install tmux on Ubuntu runners. Makefile bullseye target
  added for standing invariant checks. Net: dramatically simpler
  architecture with crash-survival, human observability, and warm pooling
  as emergent properties of the tmux substrate rather than custom code.
  All six sub-targets of 🎯T1 achieved (estimated cost 13, actual 8).
- **Deferred**:
  - /clear session-chain detection (RegisterChain seeds the initial
    entry; detecting /clear rollover to extend the chain is a follow-up)
  - `OnEvent` single-handler limitation — tracked in `STABILITY.md`
  - `Task` method renames — tracked in `STABILITY.md`

## 2026-04-19 — /release v0.7.0

- **Commit**: `4840c9f`
- **Outcome**: Released v0.7.0. Split `syscall.Flock` into
  platform-gated `flock_unix.go` (syscall.Flock) and
  `flock_windows.go` (LockFileEx via `golang.org/x/sys/windows`)
  so the `RegisterChain` / `LookupChain` sidecar machinery builds
  on Windows. The tmux-backed `Agent` is still Unix-only;
  `STABILITY.md` narrows the Windows-support caveat accordingly.
  Re-adds `golang.org/x/sys` as a direct dep (v0.6.0 had dropped
  it). Unblocks `github.com/marcelocantos/mnemo` 🎯T22 which
  needs the chain helpers to cross-compile for windows-latest.
- **Deferred**:
  - `OnEvent` single-handler limitation — tracked in `STABILITY.md`
  - `Task` method renames — tracked in `STABILITY.md`

## 2026-04-26 — /release v0.8.0

- **Commit**: `7fb1afc`
- **Outcome**: Released v0.8.0. Adds `SessionExists(sessionID,
  workDir) (bool, error)` and `SessionJSONLPath(sessionID, workDir)
  string` package-level probes so embedders can decide between
  fresh-start and resume code paths *before* invoking `Start`,
  rather than relying on the agent's silent auto-detect path. The
  motivating consumer is `meetcat resume <meeting-id>`; pageflip
  needs the explicit signal to log "specialist X starting fresh —
  no prior JSONL" instead of discovering the missing context after
  the fact. `Start`'s internal resume detection refactored through
  the new `SessionExists` so the public probe and the auto-detect
  path share one implementation. 🎯T2 achieved (estimated 2,
  actual 1).
- **Deferred**:
  - `OnEvent` single-handler limitation — tracked in `STABILITY.md`
  - `Task` method renames — tracked in `STABILITY.md`

## 2026-04-26 — /release v0.9.0

- **Commit**: `pending`
- **Outcome**: Released v0.9.0. Resolves the `claude` executable via
  a new `CLAUDE_BIN` env var (absolute or PATH-resolvable),
  `exec.LookPath`, then known install dirs (`~/.local/bin/claude`,
  `~/.claude/local/claude`, `/opt/homebrew/bin/claude`,
  `/usr/local/bin/claude`). Applied to all three spawn paths —
  Task (`task.go`), Session (`agent.go`), and Pool (`pool.go`) —
  so claudia can run under launchd / systemd / Windows Service
  whose `$PATH` excludes user-local install dirs. Surfaced the env
  var in `STABILITY.md`'s interaction-surface catalogue, and
  rewrote a stale `agents-guide.md` gotcha that previously
  asserted `claude` had to be on `$PATH`. New unit test
  `TestResolveClaudeBin` covers all five branches; existing live
  tests (`TestTaskRunSmoke`, `TestAgentSendAndWaitForResponse`,
  `TestAgentMultiTurn`, `TestRunHelper`,
  `TestAgentReadinessFailureOnDeadProcess`) confirm the resolver
  works end-to-end through both Task and Session/Pool spawn paths.
- **Deferred**:
  - `OnEvent` single-handler limitation — tracked in `STABILITY.md`
  - `Task` method renames — tracked in `STABILITY.md`

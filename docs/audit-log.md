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

- **Commit**: `pending`
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

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

# Sidecar seats and the oh-my-pi coding-agent SDK

## Decision (2026-10-19, 🎯T1074.12)

Sidecar seats run on bare `@oh-my-pi/pi-agent-core`. Claudia owns the turn
loop, context maintenance (`Maintenance` / `History` compaction), request
receipts and usage facts. The coding-agent SDK
(`@oh-my-pi/pi-coding-agent`) is not a dependency.

## What was tried

1. **Locked-down embed, v0.54.0–v0.58.0.** `sidecar/omp-session.ts` adopted
   an `AgentSession` beside the seat's own loop (`preserveSeatLoop`), with
   isolated settings, an in-memory credential store fed by the broker, the
   SDK compactor off, and the seat's stream, credential callback, queue
   preparer and turn-end hook restored after construction. Its model cache
   lived in `~omp-runtime` under the history root.
2. **Full host, branch `t1074.12-sdk-two-seats`** (8c97920, 28d5c6c,
   fe6d4ef). Each seat was hosted on the SDK's `AgentSession` and the seat
   compactor was dropped.

## Findings

- A per-seat model registry worked.
- `Settings` writes `secrets.enabled`, `providers.maxInFlightRequests` and
  `worktree.base` into module-level process state, so one seat's settings
  leak into every other seat in the sidecar.
- Config directories are resolved once per process.
- `agent_end` is re-emitted non-terminally, and the `handoff` compaction
  method never sent a terminal end.
- Auto-compaction takes no instructions; the only seam is a
  `session_before_compact` hook.
- Nine host-side adapters were needed against its queue, abort and
  compaction behaviour, and the suite still stood at 6 red plus 1
  intermittent.

## Consequences

- `omp-session.ts`, `omp-journal.ts`, their fixtures, the SDK dependency and
  its patch are removed. The `pi-ai` and `pi-agent-core` patches stay.
- Generation discovery still skips a leftover `~omp-runtime` directory
  (`legacyRuntimeDirName`) so history roots written by v0.54–v0.58 stay
  complete; nothing creates it now.
- The SDK remains a source to lift specific tools or code from, not a host.
  The parked work is on `park/omp-sdk-session` (v0.58.0) and
  `t1074.12-sdk-two-seats`.

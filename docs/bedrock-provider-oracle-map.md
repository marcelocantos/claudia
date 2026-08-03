# Bedrock Provider Oracle Map

Status: active verification plan for 🎯T12 / T12.1 / T12.2.

`ProviderBedrock` is **API-based** Claude via AWS Bedrock ConverseStream.
It is not Claude Code CLI and does not use a local `claude` binary.

Public contract (v1):

- Task: `NewTask(TaskConfig{Provider: ProviderBedrock, Model: …})` →
  `Run` → stream `TaskEventText` + terminal `TaskEventResult` / `TaskEventError`
- Session / Resume / Rewind / Cost USD / tools: **unsupported or n/a**
  (see capability matrix in [bedrock-provider.md](./bedrock-provider.md))

Live Bedrock runs are smoke/regression only (`CLAUDIA_BEDROCK_LIVE=1`).
Targets retire when hermetic fixtures, fakes, and fault checks prove the
mapping. Live residual is declared, never the sole achievement evidence.

| Target | Verification class | Machine oracle | Live role |
| --- | --- | --- | --- |
| 🎯T12.1 design + config surface | Design + env/struct docs | Design doc; no secrets; no new TOML; capability residual explicit | None |
| Provider seam | Lifecycle dispatch | `taskBackendForProvider(ProviderBedrock)` → `bedrockTaskBackend`; Session Start fail-closed | None |
| Config resolution | Deterministic env | Region/model/profile resolution unit tests (inject getenv) | Optional profile smoke |
| Request shape | Pure builder | User message + model id for ConverseStream input | None |
| Stream → TaskEvent | Fake streamer | Text deltas → `TaskEventText`; stop → `TaskEventResult` + optional Usage; errors → `TaskEventError` | None |
| Auth / config missing | Negative | Missing model/region → `Run` error without network | Live auth residual |
| Cancel | Context cancel | Interrupt cancels stream context | Live residual |
| Live smoke | Gated integration | Skip unless `CLAUDIA_BEDROCK_LIVE=1` | Work-account ConverseStream once |

## Required hermetic assets

- Pure unit tests for config + message build + event mapping (no AWS).
- Fake `bedrockStreamer` driving `Task.Run` through production backend
  wiring (`newTaskWithBackend` or streamer field).
- No golden AWS credentials. Fixtures are synthetic event sequences, not
  recorded SigV4 traffic (event-stream wire format is SDK-owned).

## Fault checks

Before 🎯T12 is retired, tests prove these fail when broken:

- Empty model id succeeds → must error.
- Empty region succeeds → must error.
- Session Start with `ProviderBedrock` succeeds → must `CapabilityError`.
- Silent tool/resume/cost capability claim → capabilities struct Task-only
  (no Session/Resume/Rewind/Cost/Tmux).
- Fake streamer text/result mapping wrong final content → oracle fails.

## Human / live residue

Machine checks do not decide:

- Whether the work account has model access enabled in Bedrock console.
- Whether the chosen model id / inference profile is available in-region.
- Billing, quotas, and cross-region inference routing correctness.
- Future tool-use parity with Claude CLI Task.

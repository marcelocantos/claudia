# Grok Provider Oracle Map

Status: active verification plan for 🎯T7.8.

Grok provider work harnesses the **Grok Build CLI** (`grok`), not the
Realtime voice client in package `github.com/marcelocantos/claudia/grok`.

Public contracts:

- Headless Task: `grok -p … --output-format streaming-json`
- Persistent Session (planned): `grok agent stdio` (ACP JSON-RPC)

Live Grok runs are smoke/regression only (`CLAUDIA_GROK_LIVE=1`). A target
retires when hermetic fixtures, fakes, and fault checks prove the mapping.

| Target | Verification Class | Machine Oracle | Live Role |
| --- | --- | --- | --- |
| 🎯T7.1 provider seams | New-code lifecycle seam | Backend dispatch tests select `grokTaskBackend` / `grokAgentBackend` without Claude/Codex leakage. | None |
| 🎯T7.2 binary discovery | Deterministic resolver | `TestResolveGrokBin` injects env, PATH, `~/.grok/bin/grok`, missing-binary failure. | Optional install sanity |
| 🎯T7.3 Grok Task mode | Public CLI fixture parser | Golden `testdata/grok/exec/*` + `grokTaskParser` + `TestGrokTaskSuccessOracleRejectsFaults`. | `CLAUDIA_GROK_LIVE=1` smoke only |
| 🎯T7.4 Session contract spike | Public protocol fixture/schema | ACP stdio capture fixtures (initialize / session/new / session/prompt / session/update). | Explicit approved live capture |
| 🎯T7.5 Grok Session mode | Fake ACP lifecycle | Fake stdio ACP drives Start/Send/Wait/Subscribe/Interrupt hermetically. | Gated live smoke only |
| 🎯T7.6 capability gaps | Negative capability oracle | `TestStartGrokSessionFailsWithCapabilityError`, `TestGrokRewindFailsWithCapabilityError`, plus cost/tool-stream gaps when declared. | Human review of accepted gaps |
| 🎯T7.7 docs/release gate | Documentation consistency | README, agents-guide, STABILITY, release notes share one support matrix. | Release checklist only |

## Required Golden Fixtures

`testdata/grok/exec/`:

- `success.jsonl`: thought, multi-chunk text, end with sessionId.
- `error.jsonl`: `{"type":"error","message":…}`.
- `malformed.jsonl`: invalid JSON + unknown types + a valid trailing stream.

`testdata/grok/acp/` (when T7.4 lands):

- initialize handshake, session/new, session/prompt, agent_message_chunk stream, error path.

## Fault Checks

Before 🎯T7 is retired, tests must prove these injected faults fail:

- Dropped session id on `end` (`TestGrokTaskSuccessOracleRejectsFaults`).
- Wrong final-message selection (mutated text chunks).
- Silent unsupported capability success (Session Start / Rewind).
- Private-storage shortcut: production code must not drive Session by
  truncating `~/.grok/sessions` files.

## Human Residue

Machine checks do not decide:

- Whether missing headless tool_use events are acceptable vs Claude Task.
- Whether ACP maturity is enough for Session mode.
- Whether `ProviderGrok` naming collides with consumer mental models of
  package `claudia/grok` (docs must disambiguate).

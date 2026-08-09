# Codex Provider Oracle Map

Status: **sealed** for 🎯T4.8 (machine checks green on local master).

Codex provider work is mostly new-code oracle mode with two public-contract seams:

- `codex exec --json` for Task mode.
- `codex app-server` JSON-RPC for persistent Session mode (production Start remains fail-closed experimental until 🎯T4.4 live contract proof).

Live Codex runs are smoke/regression only. They do not retire targets by themselves. A target retires when hermetic fixtures, fakes, and fault checks prove the mapping and lifecycle behavior.

## Canonical hermetic command (T4.8)

```bash
go test -count=1 -timeout 120s -run \
  'TestCodex|TestFakeCodex|TestParseCodex|TestStartCodex|TestHermeticTaskRunCodex|TestResolveCodex|TestPreflightCodex|TestEnsureCodex|TestCapabilityError|TestExperimentalCapability|TestAgentMissing' \
  .
```

Live residual (never sole retirement evidence): `CLAUDIA_CODEX_LIVE=1` → `TestCodexTaskRunSmoke`.

## Claim → oracle matrix

| Target | Verification class | Machine oracle | Live role | Status |
| --- | --- | --- | --- | --- |
| 🎯T14.1 binary + subscription foundation | Resolver + auth preflight + live spike | `TestResolveCodexBin*`, `TestCodexBinCandidatesIncludeDesktopAppBundle` (ChatGPT.app + Codex.app), `TestPreflightCodexAuth*`, `TestEnsureCodexSubscriptionAuth`, hermetic `TestHermeticTaskRunCodexSpawn` with fixture auth. Spike: [codex-subscription-spike.md](codex-subscription-spike.md). | One-shot `codex exec --json` on ChatGPT OAuth; `CLAUDIA_CODEX_LIVE=1` Task smoke | Hermetic sealed |
| 🎯T4.1 provider boundaries | New-code lifecycle seam | Fake Claude / fake Codex task+agent backends drive the same public lifecycle (`TestStartUsesInjectedBackendLifecycle`, fake-codex Task path) without real binaries. | None | Hermetic sealed |
| 🎯T4.2 Codex binary discovery | Deterministic resolver | Resolver tests inject env, PATH lookup, app-bundle candidates, missing-binary failure. | Optional manual install sanity | Hermetic sealed |
| 🎯T4.3 Codex Task mode | Public CLI fixture parser + hermetic spawn | Golden `codex exec --json` fixtures + `TestCodexTaskParser*` + `TestCodexTaskSuccessOracleRejectsFaults` + `TestHermeticTaskRunCodexSpawn`. | `CLAUDIA_CODEX_LIVE=1` smoke only | Hermetic sealed |
| 🎯T4.4 app-server contract spike | Public protocol fixture/schema | Golden app-server JSON-RPC fixtures + `TestParseCodexAppServer*` + `TestCodexAppServerFixturesAreValidJSONL`. | Explicit live capture residual | Hermetic fixtures sealed; live capture human residual |
| 🎯T4.5 Codex Session mode | Fake app-server lifecycle | `TestFakeCodexAppServerLifecycle`, `TestFakeCodexAppServerInterruptLifecycle` (Start/Send/Wait/Subscribe/Interrupt/raw/usage/attach-log fail-closed). Production `ProviderCodex` Session Start remains experimental fail-closed. | Gated live app-server smoke only | Fake harness sealed |
| 🎯T4.6 capability gaps | Negative capability oracle | `TestStartCodexSessionFailsWithCapabilityError`, `TestCodexRewindFailsWithCapabilityError`, `TestAgentMissingOperationFailsWithCapabilityError`, attach/log empty on fake, `TestCodexProviderCapabilitiesClaimed`. | Human review of accepted gaps | Hermetic sealed |
| 🎯T4.7 docs/release gate | Documentation consistency | README, agents-guide.md, STABILITY.md, release notes share one support matrix (blocked on code targets + this map). | Release checklist only | Map ready; release gate waits on T4.x code |
| 🎯T4.8 verification choke | Oracle-first map + assets | This document + fixture inventory + fault oracles below. | Live never sole evidence | **Sealed** |

## Required golden fixtures

`testdata/codex/exec/`:

- `success.jsonl`: `thread.started`, `turn.started`, `item.started` command, `item.completed` agent messages, `turn.completed` usage.
- `failure.jsonl`: `thread.started`, `turn.started`, `turn.failed`.
- `error.jsonl`: top-level `error`.
- `malformed.jsonl`: invalid JSON and unknown event types (ignored per parser contract).

`testdata/codex/app-server/`:

- `success.jsonl`: initialize response, thread/start response, turn/start response, item notifications, agent message, turn/completed + usage.
- `failure.jsonl`: turn failure.
- `interrupted.jsonl`: turn/started, turn/interrupt, turn/completed interrupted.
- `unsupported-capability.jsonl`: method/field rejection for experimental capability.
- `thread-start.jsonl`: redacted live-shape thread start (JSONL validity + token check).

Fixtures must stay small, redacted, and hand-owned. `TestParseCodexAppServer*`
parses them into typed events so notification order and field mapping are
machine-checked. `TestFakeCodexAppServer*` drives the Agent lifecycle through
an injected app-server-shaped backend. `TestCodexOracleFixturesPresent` ratchets
that every required fixture path still exists.

## Fault checks

Machine oracles prove these injected faults fail:

| Fault | Oracle |
| --- | --- |
| Dropped thread/session id | `TestCodexTaskSuccessOracleRejectsFaults` |
| Wrong final-message selection | `TestCodexTaskSuccessOracleRejectsFaults` |
| Malformed usage accounting | `TestCodexTaskSuccessOracleRejectsFaults` |
| Silent unsupported capability success | `TestStartCodexSessionFailsWithCapabilityError`, `TestCodexRewindFailsWithCapabilityError`, `TestAgentMissingOperationFailsWithCapabilityError`, fake app-server attach/log empty |
| Private-storage shortcut | `TestCodexProviderDoesNotReadPrivateStorage` scans production Go for private Codex state path tokens |

## Human / live residue

Machine checks do **not** decide:

- Whether Codex parity gaps versus Claude Code are acceptable for users.
- Whether public API names feel right before v1.0.
- Whether app-server maturity risk is acceptable for enabling production Session mode (🎯T4.4 / 🎯T4.5 product enablement).
- Live ChatGPT subscription rate-limit / quota behavior under fleet load (related: 🎯T14.4).

Those require one explicit human accept/reject review after the machine checks pass. Live gates remain optional smoke, never sole retirement evidence.

## Relationship to other maps

- Claude: [claude-provider-oracle-map.md](claude-provider-oracle-map.md) (🎯T11)
- Grok: [grok-provider-oracle-map.md](grok-provider-oracle-map.md) (🎯T7.8)
- Bedrock: [bedrock-provider-oracle-map.md](bedrock-provider-oracle-map.md) (🎯T12)
- Subscription foundation spike: [codex-subscription-spike.md](codex-subscription-spike.md) (🎯T14.1)

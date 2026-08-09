# Codex Provider Oracle Map

Status: **sealed** for 🎯T4.8 (machine checks green on local master).

Codex provider work is mostly new-code oracle mode with two public-contract seams:

- `codex exec --json` for Task mode.
- `codex app-server` JSON-RPC for persistent Session mode (🎯T4.4 contract proven 2026-08-09; production Start still experimental until 🎯T4.5 wires it).

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
| 🎯T14.2 `codex` Task subpackage | Package Task API + typed failures + fixtures | `go test ./codex/`: `TestParse*Fixture`, `TestClassifyFailure*`, `TestHermeticTaskRunSuccess`, `TestHermeticTaskRunRateLimit`, `TestHermeticTaskRunAuthFailJSONL`, `TestHermeticTaskRunNonZeroExit`, `TestHermeticTaskAuthPreflightFails` | `CLAUDIA_CODEX_LIVE=1` / `CLAUDIA_LIVE=1` → `TestLiveCodexTaskRun` | Hermetic sealed |
| 🎯T4.1 provider boundaries | New-code lifecycle seam | Fake Claude / fake Codex task+agent backends drive the same public lifecycle (`TestStartUsesInjectedBackendLifecycle`, fake-codex Task path) without real binaries. | None | Hermetic sealed |
| 🎯T4.2 Codex binary discovery | Deterministic resolver | Resolver tests inject env, PATH lookup, app-bundle candidates, missing-binary failure. | Optional manual install sanity | Hermetic sealed |
| 🎯T4.3 Codex Task mode | Public CLI fixture parser + hermetic spawn | Golden `codex exec --json` fixtures + `TestCodexTaskParser*` + `TestCodexTaskSuccessOracleRejectsFaults` + `TestHermeticTaskRunCodexSpawn`. | `CLAUDIA_CODEX_LIVE=1` smoke only | Hermetic sealed |
| 🎯T4.4 app-server contract spike | Public protocol fixture/schema | Golden fixtures (`success`/`failure`/`interrupted`/`unsupported`/`thread-start`/`live-turn`/`lifecycle`) + `TestParseCodexAppServer*` + request builders + private-storage scan. Spike: [codex-app-server-spike.md](codex-app-server-spike.md). | Live 2026-08-09 capture on codex-cli 0.146.0-alpha.9.2 (turn + resume/fork/archive/interrupt) | **Sealed** (live + hermetic) |
| 🎯T4.5 Codex Session mode | Fake app-server lifecycle | `TestFakeCodexAppServerLifecycle`, `TestFakeCodexAppServerInterruptLifecycle` (Start/Send/Wait/Subscribe/Interrupt/raw/usage/attach-log fail-closed). Production `ProviderCodex` Session Start remains experimental fail-closed. | Gated live app-server smoke only | Fake harness sealed |
| 🎯T4.6 capability gaps | Negative capability oracle | Matrix: `TestCodexCapabilityMatrixIsExplicit`, `TestCodexCapabilityGapsVersusClaude`, `TestCodexCapabilityMatrixMatchesBackendClaims`, `TestProviderCapabilityMatrixIsTotal`, `TestCheckCapabilityFailsClosed`, `TestCodexProviderCapabilitiesClaimed`. Fail-closed call sites: `TestStartCodexSessionFailsWithCapabilityError`, `TestCodexRewindFailsWithCapabilityError`, `TestAgentMissingOperationFailsWithCapabilityError`, `TestCodexTaskToolRestrictionsFailClosed`, `TestCodexTaskWithoutRestrictionsIsNotBlocked`, `TestCodexTaskArgsCarryNoToolRestrictionFlags`, attach/log empty on fake. | Human review of accepted gaps | Hermetic sealed |
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
- `live-turn.jsonl`: redacted live 0.146 full turn (camelCase items, `thread/tokenUsage/updated`, sandbox/approval map).
- `lifecycle.jsonl`: redacted resume/fork/archive/unarchive/interrupt shapes.

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
| Silently dropped `DisallowTools` on Codex Task | `TestCodexTaskToolRestrictionsFailClosed` (typed refusal before spawn) + `TestCodexTaskArgsCarryNoToolRestrictionFlags` (no forged Claude flag Codex would ignore) |
| Over-broad refusal masquerading as a guard | `TestCodexTaskWithoutRestrictionsIsNotBlocked` — an unrestricted Codex task must still reach binary/auth resolution |
| Undeclared capability gap | `TestProviderCapabilityMatrixIsTotal` (every provider claims every reported capability, with a reason) + `TestCodexCapabilityGapsVersusClaude` (head-to-head against `ProviderClaude`) |
| Matrix/backend drift | `TestCodexCapabilityMatrixMatchesBackendClaims` — a wired-but-unclaimed or claimed-but-unwired capability fails |
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

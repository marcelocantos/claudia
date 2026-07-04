# Broker oracle map

The broker (🎯T2) is a concurrent, timing-driven, control-loop system with
almost no external referent. That makes it **new-code oracle mode**: we *author*
the correctness spec as executable checks, rather than diffing against a
reference implementation. The dominant human cost is verification-*judgment*,
and the trap is verifying policy by running the live daemon and watching 429s,
idle timers, and preemptions — a class-4 dynamical signal that is
non-reproducible and worth ~1 bit per round-trip.

This document is the choke-point declared by 🎯T2.0: it maps every broker policy
target to its verification class and the specific machine oracle that gates it,
so **no policy sub-target is ever accepted on live-observation evidence**. Each
oracle is seeded here before the policy code it guards is written.

## The two seams that make policy testable

Everything below depends on two injectable seams, both delivered by T2.0:

| Seam | Where | What it removes |
|---|---|---|
| **Clock** (`internal/broker/clock.go`) | `ManualClock` in tests | Wall-clock waiting. All timing decisions read `Clock`; `policy_guard_test.go` fails the build if any policy file calls `time.Now/After/Sleep` directly. |
| **Fake `claude`** (`internal/broker/brokertest`) | built per test binary | Real API credit. Emits canned JSONL turns, an injectable 429, and a readiness marker on demand, so AIMD / cost / reaping / preemption run headless. |

## Oracle per target

| Target | Class | Gating oracle | Load-bearing property | Status |
|---|---|---|---|---|
| **T2.1** protocol / socket | 1 decidable | Golden message vectors (round-trip encode/decode) + one real-socket spawn/release integration test | Wire format is stable; `CLAUDIA_NO_BROKER=1` fallback is byte-identical to today's direct path | seam ready; vectors TODO with T2.1 |
| **T2.2** AIMD | 4 dynamical → 1 | Deterministic simulator: seeded 429 tapes (from the fake) + `ManualClock`; assert never-exceed-cap, halve-on-429, additive-recover, cap ≥ 1 | **No N×K cascade** — aggregate in-flight never exceeds the adapting cap under a simulated rate-limit regime | seams ready; sim TODO with T2.2 |
| **T2.4** cost | 2 reference-comparable | Differential: local token estimate vs Anthropic Cost API reconciler; gate on **bounded drift** `|local − reconciler| < ε` over a window — **not** equality (the reconciler lags ~5 min) | Real-time estimate tracks ground truth within ε; never blocks on reconciler absence | seams ready; diff TODO with T2.4 |
| **T2.5** idle reaping | 1 decidable | `ManualClock`: advance to each model-weighted threshold, assert reap fires exactly then | opus reaps before sonnet before haiku, at the declared TTLs | **oracle ready** (`ManualClock` + tests) |
| **T2.6** preemption | state machine | **Model-checked** lifecycle spec (`specs/AgentLifecycle.tla`), seeded before implementation | **No double-ownership**; **no Send-after-reap**; the SIGTSTP-drain and heartbeat-timeout races resolve safely | **spec seeded + mutation-proven** |

`make verify-specs` runs the T2.6 spec: the correct config is green, and two
fault-injection mutants (reap-a-held-agent, steal-grant) are each caught by the
matching invariant. A spec that stays green on known-broken code is toothless;
the mutation catch-rate is how we measure that these invariants have teeth
(oracle-first rule 12). CI enforces it in `.github/workflows/specs.yml`.

## Shakeout-clock oracle (🎯T1.6, separate epic)

"No backwards-incompatible public-API change since v0.12.0" is class-1 decidable:

```
gorelease -base=v0.12.0    # reports the required semver bump; must be < major
```

The broker lands entirely under `internal/`, so it adds nothing to the public
API and does not disturb the shakeout clock — `gorelease` reports a compatible
(additive) change.

## The residual — what no oracle here can certify

The oracles above certify the code obeys the declared policy. They **cannot**
certify the policy is the *right* one (oracle-first rule 11). These are explicit
accepted risk, gated by dogfooding and a single human accept/reject, not by any
green suite:

- Is **AIMD** the right controller for Anthropic's rate-limit behaviour, or does
  it oscillate / underutilise in practice?
- Are the **model-weighted TTLs** (opus 5m / sonnet 15m / haiku 60m) the right
  reap thresholds?
- Is **intent inference** (mode + recency → priority tier) the right priority
  model, or does it mis-tier real workloads?

Stabilise these by use before hardening; when settled, record the outcome here
rather than leaving the risk silent.

# Broker oracle map

The broker (🎯T2) is a concurrent, timing-driven, control-loop system with
almost no external referent. That makes it **new-code oracle mode**: we *author*
the correctness spec as executable checks, rather than diffing against a
reference implementation. The dominant human cost is verification-*judgment*,
and the trap is verifying policy by running the live daemon and watching 429s,
idle timers, and preemptions — a class-4 dynamical signal that is
non-reproducible and worth ~1 bit per round-trip.

This document is the choke-point declared by 🎯T2.0 and completed by 🎯T2.8: it
maps every broker policy target to its verification class and the specific
machine oracle that gates it, so **no policy sub-target is ever accepted on
live-observation evidence**. Each oracle is seeded here before the policy code
it guards is written — 🎯T2.2/🎯T2.4/🎯T2.5/🎯T2.6 are still unimplemented, and
that ordering is deliberate: the seams below constrain how that policy code is
allowed to read time and backpressure when it lands.

## The three seams that make policy testable

Everything below depends on injectable seams. Policy code reads time and
backpressure through these and nothing else, and a build-failing guard keeps it
that way:

| Seam | Where | What it removes |
|---|---|---|
| **Clock** (`internal/broker/clock.go`) | `ManualClock` in tests | Wall-clock waiting. Every timing decision reads `Clock`, so a model-weighted TTL is exercised in microseconds instead of minutes. |
| **BackpressureSource** (`internal/broker/backpressure.go`) | `ScriptedBackpressure` in tests | Live 429 observation. Policy consumes a `Signal` stream; `ClassifyResultLine` is the one place a 429's wire shape is interpreted, and `JSONLBackpressure` is the only thing that reads claude's output. |
| **Fake `claude`** (`internal/broker/brokertest`) | built per test binary | Real API credit. Emits canned JSONL turns, canned usage blocks, an injectable 429, and the real readiness prompt-box frame, so AIMD / cost / reaping / preemption run headless. |

### The guard that makes them the *only* way

`internal/broker/policy_guard_test.go` scans every declared policy path and
fails the build on a direct wall-clock or live-socket read. Two things keep it
honest:

- **It is tested in both directions.** `scanPolicySource` is a pure function
  with a table test covering what must trip it (`time.Now(`, `time.Since(`,
  `time.Sleep(`, `time.After(`, `time.NewTicker(`, `http.Get(`,
  `http.DefaultClient`, `net.Dial`) *and* what must not (`c.Now()`,
  `time.Unix(`, `time.Duration(secs)`, `5 * time.Minute`, `time.Time` in a
  signature). An over-broad rule gets switched off within a week, so
  over-broadness is itself a test failure.
- **Enrolment cannot silently shrink.** `internal/broker` is policy by
  construction (minus the `clock.go` seam). Files elsewhere opt in with a
  `//claudia:policy` marker, and `pool.go` is pinned by name — deleting its
  marker fails the build rather than quietly emptying the guarded set.

`pool.go` was the pre-existing offender: it read `time.Now().Unix()` inline to
decide whether a warm window had expired, and again to stamp a
`keep_alive_for:<secs>` deadline. Both now read the injected `poolClock`, and
the two decisions are extracted as `poolWindowExpired` / `poolKeepAliveDeadline`
so `pool_clock_test.go` can pin the exact expiry boundary (live at +299s,
expired at +300s) and the "a parse slip must never license a kill" direction,
under a `ManualClock`, with no tmux and no sleeping.

## Oracle per target

| Target | Class | Gating oracle | Load-bearing property | Status |
|---|---|---|---|---|
| **T2.1** protocol / socket | 1 decidable | Golden message vectors (round-trip encode/decode) + one real-socket spawn/release integration test | Wire format is stable; `CLAUDIA_NO_BROKER=1` fallback is byte-identical to today's direct path | seams ready; vectors TODO with T2.1 |
| **T2.2** AIMD | 4 dynamical → 1 | Deterministic simulator: seeded 429 tapes (from the fake) + `ManualClock`; assert never-exceed-cap, halve-on-429, additive-recover, cap ≥ 1 | **No N×K cascade** — aggregate in-flight never exceeds the adapting cap under a simulated rate-limit regime | seams ready; sim TODO with T2.2 |
| **T2.4** cost | 2 reference-comparable | Differential: local token estimate vs Anthropic Cost API reconciler; gate on **bounded drift** `|local − reconciler| < ε` over a window — **not** equality (the reconciler lags ~5 min) | Real-time estimate tracks ground truth within ε; never blocks on reconciler absence | seams ready; diff TODO with T2.4 |
| **T2.5** idle reaping | 1 decidable | `ManualClock`: advance to each model-weighted threshold, assert reap fires exactly then | opus reaps before sonnet before haiku, at the declared TTLs | **oracle ready** (`ManualClock` + tests) |
| **T2.6** preemption | state machine | **Model-checked** lifecycle spec (`specs/AgentLifecycle.tla`), seeded before implementation | **No double-ownership**; **no Send-after-reap**; the SIGTSTP-drain and heartbeat-timeout races resolve safely | **spec seeded + mutation-proven** |
| **T2.8** oracle layer | 1 decidable | The guard, seam and double tests themselves: `go test -race ./internal/broker/...` + `make verify-specs` | Policy cannot read time or backpressure except through a seam; the double emits what the product emits; every spec mutant is caught | **delivered** |

`make verify-specs` runs the T2.6 lifecycle spec. The correct config is green,
and three fault-injection mutants are each caught by the matching invariant —
mutation is how we measure that the invariants have teeth (oracle-first rule
12), because a spec that stays green on known-broken code certifies nothing:

| Mutant config | Injected fault | Invariant that must catch it |
|---|---|---|
| `AgentLifecycle_mutant_steal.cfg` | grant an already-held agent to a second consumer | `Inv_NoDoubleOwnership` |
| `AgentLifecycle_mutant_reap.cfg` | reap a preempted agent the broker still records as held | `Inv_NoHeldReap` |
| `AgentLifecycle_mutant_stale_handle.cfg` | reclaim without invalidating the consumer's handle | `Inv_NoSendAfterReap` |

The third is the one worth explaining. The spec keeps two views of ownership
apart, because their disagreement *is* the bug class: `held[a]` is the broker's
record, `handles[c]` is what a consumer believes it may still Send to. The
reaper consults `held` — it cannot see inside a consumer — so a reclaim that
drops the broker's record without invalidating the handle leaves a live handle
to an agent the broker is now free to tear down. The agent then reaps
*legitimately*, and the next `Send` lands on a dead session. Stating the
invariant over an explicit `Send` action rather than over handle-set hygiene
means it fails on the act itself, not on a proxy for it.

CI enforces all four runs in `.github/workflows/specs.yml`.

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

There is a smaller, structural residual too. The guard scans this package plus
marker-carrying files in the root package; a policy file added in a *new*
package without the marker is not scanned until it is enrolled. The pinned-file
check catches erosion of what is already declared, not omission of something
new — that stays a review-time obligation.

Stabilise these by use before hardening; when settled, record the outcome here
rather than leaving the risk silent.

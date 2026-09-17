# Claudia as a metaharness

Status: design record for the 🎯T2 reframe (owner decision, 2026-09-12).
First implementation landed the same day: `claudia broker serve`
(now package `daemon`), the grant wire (`internal/broker/wire_grants.go`),
the socket-backed Session and Task backends (`broker_agent.go`,
`broker_task.go`), and the usage evaluator (now `PlanUsageMonitor`,
`plan_usage_monitor.go`). Operator surface: `cmd/claudia`.
It follows on from [plan-usage.md](plan-usage.md) (🎯T61) and
[broker-oracles.md](broker-oracles.md) (🎯T2.8).

## The sentence

**Claudia is the process that runs agents and keeps their plan-contract
true. Clients name seats and write turns. They do not evaluate usage and
they do not pick models turn by turn.**

Not "a library for embedding agent CLIs" (README today), not a Claude
process allocator (🎯T2 as first written), and not an OpenRouter.

## Why a daemon, stacked

🎯T2 was filed for rate-limit slots, warm-pool waste, and cross-process
cost. Three more reasons landed in September 2026, each one a thing a
library cannot do.

| Need | Why a library cannot own it | Target |
|---|---|---|
| Plan usage is host-global | 🎯T61's flock+TTL cache stops five processes hitting vendor endpoints, but the evaluator still runs inside whichever client won the flock. That client can stall the host picture, stampede a refresh, die mid-fetch, and cannot invalidate on a 429 it never saw. | 🎯T2.9 |
| Consumer upgrade without a fleet bounce | Jevons pays for the inverse: a daemon-path rebuild restarts `jevonsd`, then T171 rehydrates POs and workers and the fleet is amnesiac rather than dead. Only Claude (tmux) and Grok (connect-mode `serve`) can be re-adopted; Cursor `agent acp` and `codex app-server` are stdio children and die with the consumer (`registry.go` `startLifecycle`, adopt switch → `ErrNoSessionWindow`). | 🎯T2.11 |
| Standing contract on a seat | A process owner can keep "this grant satisfies these predicates, including has-tokens" true for every consumer at once. A library that switched model mid-call unasked would surprise every consumer that is not the fleet, so in the library the policy is opt-in (🎯T75). | 🎯T2.12 |

The cache in 🎯T61 is the brokerless fallback for the first row. It is
not the architecture.

## Library first (🎯T75, owner 2026-09-17)

Some applications embed claudia as a library with no daemon, so the
split below is a split of *scope*, not of *code*. Everything that can
feasibly be implemented in the library is, future work included, and the
daemon exposes it by pass-through.

What only a server gives, and so stays in `claudia/daemon`:

- the socket and the wire (`internal/broker`);
- one owner per seat at a time (`grant_held`, `not_owner`);
- a seat that outlives its consumer, with the in-flight stream retained
  for whoever reclaims it by name;
- one usage evaluator for the whole host;
- service install and the `claudia broker` CLI.

Everything else is library API the daemon calls:

| Capability | Library | Daemon's part |
|---|---|---|
| Seat resume after a restart, nudge, remint | `Registry.ResumeAll` | calls it on boot, maps outcomes onto the tail |
| Seat lifecycle events, liveness watch | `Registry.SubscribeSeatEvents` | forwards to the tail and to the owner (`agent_gone`) |
| Migrate persistence | `Registry` records `Migrate` on its seats | nothing |
| Rewind | `Registry.Rewind` | `rewind` op, moves the grant's subscription |
| Plan-usage monitor, 429 invalidation | `PlanUsageMonitor`, invalidation on a direct agent's stuck event | runs one instance host-wide |
| MCP hosting | `MCPHost`, `Registry.SetMCPHost` | runs one host, sets `jevonsmcp` as consumer-owned |
| Model-intel refresh | `RunModelIntelRefresher` | runs it |
| Goal completeness | `Config.GoalCompleteCheck` | asks the owning handle (`goal_check`) |
| Task raw lines | `Task.SetRawLog` | pushes `task_raw` |
| Warm pool | `AcquireDirect` / `Agent.Release` | runs the pool for every consumer (`grant.pool`) |

Auto-actuating policy not yet built (rebind 🎯T2.12, AIMD 🎯T2.2, reaping
🎯T2.5, preemption 🎯T2.6, cost 🎯T2.4, adaptive pool 🎯T2.3) is library
code too: opt-in and off by default in direct mode, switched on by the
daemon for the seats it holds.

The boundary is structural: the daemon is a separate package that
compiles only against exported API, and `make gate` fails if it imports
any other package of this module (`daemon/imports_test.go`).

## Responsibility split

Scope, per the section above: the mechanism in each row is library code.

| | Daemon (metaharness) | Client (Jevons, YTT, …) |
|---|---|---|
| Lifecycle | spawn, grant, reclaim, reap, preempt | request a seat: name, purpose, parent, workdir |
| Usage | track, classify, report | read |
| Routing | `Resolve` on grant; rebind when the snapshot would violate the grant | set predicates once; pin if needed |
| MCP / tools | host the connections; rewrite the grant to loopback URLs (🎯T2.16) | choose the config (jevonsmcp stays the consumer) |
| Prompt / Goal / UI | execute | decide content and fleet topology |

"Switch when tokens run low" is on the daemon **only** as
rebind-to-keep-predicates. Jevons Claude-first is `PreferProvider=claude`
on the grant. Overseer exemption is a pinned grant. Park-vs-migrate stays
a client decision: the daemon says "this grant cannot be satisfied"
(`stuck`); only Jevons parks a seat. That moves actuation without
importing Jevons policy, so 🎯T13 holds.

## What the native wire has to be

Wire v1 (🎯T2.1) is `spawn` / `release` / `status` / `tail`.
`SpawnRequest` carries provider, mode, model, intent, workdir. No name,
no parent, no MCP, no predicates, no `send`, no events. That is a
process allocator.

The consumer census says what the wire must carry instead. Counts are
symbol uses in the Jevons tree:

| Symbol | Uses |
|---|---|
| `claudia.AgentDef` | 346 |
| `claudia.Event` | 226 |
| `claudia.NewRegistry` / `Registry` | 277 |
| `claudia.Agent` | 59 |
| `claudia.Config` | 3 |

Jevons speaks **Registry by name plus the Event stream**, not `Start`.
So the native API over the socket is the Registry contract
(`grant` / `reclaim` / `release` keyed by name, carrying the `AgentDef`
fields), `send` / `interrupt` / `wait`, and a per-grant `event` stream
carrying the public `Event` type verbatim. STABILITY.md's Event row is
the wire schema. Adding `usage` and `resolve` requests makes the
daemon the evaluator (🎯T2.9). This is 🎯T2.10; it is where `agent.go`
splits into a daemon-side backend and a client-side handle.

It is NDJSON over a Unix socket with JSON types that already have
golden vectors. Any language can speak it; Go gets it for free through
the existing API (🎯T3).

## Published protocol: three layers, one native

1. **Native (the product).** The claudia Go surface over the socket,
   as above. It is allowed to look like Claudia.
2. **ACP as a Session facade** (🎯T2.13). Claudia already speaks ACP as
   a client to Grok and Cursor. Serving it lets an IDE attach to **one
   grant** as if it were one agent. ACP is a bad control plane for a
   fleet: one session, one agent, no grants, no host-wide usage, no
   "rebind this seat".
3. **OpenAI-compatible as a Task facade** (🎯T2.13). Prompt in, stream
   out, `Resolve` picks when `model` is empty. No MCP, no session. Do
   not stretch it to Session.

Codex app-server and Grok/Cursor ACP stay backend dialects the daemon
speaks as a client.

Why not OpenRouter: OpenRouter is many model HTTP APIs behind one chat
schema, keys and billing. Claudia is many agent harnesses plus a few
direct routes, subscription plans, a workdir, tools, and MCP. MCP is the
tell: tools run inside the harness Claudia started. Claudia routes
**agents**, not completions.

## Rebind semantics (🎯T2.12)

- Evaluated at exactly one point: `send` admission on a grant whose
  current `(provider, model)` fails `HasAvailableTokens` or a `NotBand`
  predicate against the daemon snapshot. Never on a timer. Never while
  `PromptInFlight`.
- Intra-provider → `SetModel` (🎯T54). Cross-provider → `Agent.Migrate`
  with the inert seed (🎯T55.1). `ProgressType=model_switch` is
  published before the admitted Send's first Event, with the `Resolve`
  reason.
- Pinned grant → never rebound. No satisfiable candidate → `stuck`
  Event with `StuckClass`, Send refused with a typed error.
- The mechanism is library code (🎯T75). It is opt-in and off by
  default in direct mode: a library application that has not enabled it
  never has its model switched (same predicates, hot band → `stuck`, no
  switch). The daemon enables it for the grants it holds.
- `AgentLifecycle.tla` gains a Rebind action: ownership unchanged,
  backend identity changes, no Send lost or duplicated, no rebind while
  a turn is open. A mutant that rebinds mid-turn must be caught.

Preemption (🎯T2.6) shares this admission point. "tmux suspend-pane" is
a Claude-only mechanism; for stdio backends, pause is refusing Send
admission until capacity returns.

## Delivery order, risk-ranked

| Step | Target | Touches spawn? | Why this order |
|---|---|---|---|
| 1 | 🎯T2.9 usage service | no | Same coordinator as 🎯T2.4 with plan windows as a second unit. 🎯T61 becomes the fallback. Jevons and YTT gain it through a library upgrade. |
| 2 | 🎯T2.10, Task first | yes, request-scoped | A Task is a single turn; ownership is simplest. |
| 3 | 🎯T2.10, Session grants | yes | The big one: Registry over the socket, Event over the wire, daemon parents stdio children. |
| 4 | 🎯T2.11 reclaim across every provider | yes | Falls out of step 3 once the daemon is the parent; the value is the Jevons restart journey. |
| 5 | 🎯T2.12 rebind | no new spawn | Needs 1 and 3 plus the 🎯T2.6 admission state. |
| 6 | 🎯T2.13 facades | no | Adapters over one grant or one Task. |

🎯T3 widens to match: `Registry`, `LoadPlanUsage`, and `Resolve` take
the socket when present. A spawn-only RPC client would leave Jevons on
the direct path. 🎯T47.6 (probe-then-ignore) is subsumed when 🎯T3 lands.

## What does not change

- Direct mode stays a fully supported path with the same capabilities
  (🎯T3, 🎯T13, 🎯T75). No socket, `CLAUDIA_NO_BROKER=1`, or
  `SetDirect` / `StartDirect` / `AcquireDirect` → in-process; no
  auto-actuation unless the application enables it.
- 🎯T2.8's seams still gate every policy path: Clock, BackpressureSource,
  the brokertest fake. Usage refresh and rebind read time and 429s only
  through them.
- The 🎯T1.6 shakeout clock is not reset; the broker path is additive.

## Reboot recovery (🎯T2.14)

The daemon marks every seat it grants as live in its own registry
(`grants.json` under the state dir, `AutoStart`). On boot it walks that
set: `Adopt` first (a tmux window or connect-mode serve that survived a
daemon-only restart), else `Launch` with the registry's resume rules
(`Materialized` → `RequireResume`, fail-closed). A relaunched seat is
sent a restart nudge before any consumer reconnects, so the agent knows
its tool calls did not finish. Consumers reconnect by granting the same
names; Jevons's boot path (`ReattachFleet`) already does that, and with
the daemon present it no longer stops or reaps seats on its own exit.

## Residue

- Upgrading the daemon itself still bounces the fleet unless the daemon
  can detach from its children. The win is making that bounce rare:
  Claudia's wire changes slowly; consumer policy does not.
- The warm pool is still a Claude tmux mechanism. 🎯T64 serves it
  through the daemon as it is; 🎯T2.3 generalises the library pool, and
  🎯T78 makes a pooled agent publish its turn events.

## Handoff (2026-09-12, end of session)

State at HEAD: `claudia broker serve` is installed as a launchd user
agent (`com.marcelocantos.claudia-broker`, binary `~/go/bin/claudia`,
log `~/.local/state/claudia/broker.log`, grants in
`~/.local/state/claudia/grants.json`). Jevons (69bc2499) and ytt
(4dcb3f1) consume it through the library; both carry
`replace github.com/marcelocantos/claudia => ../claudia` until the next
claudia tag.

Ledger: 🎯T3, 🎯T2.9, 🎯T2.10, 🎯T2.11, 🎯T2.14 achieved. T2.10 vcheck
PASS at 3025491 (hermetic 18/18, wire vectors, `make gate` ✓✓✓✓ on
the checker's second attempt; live Session/Task/reclaim through the
launchd daemon in scratchpad/live-3025491.log). T2.11 and 🎯T2.14
recorded on that PASS using the attestations drafted 2026-09-12
(vcheck PASS at 940a66e; hermetic/live reconfirmed at 3025491).
bullseye.yaml is dirty — commit the ledger with the next code change.

Next: 🎯T2.7 (brew-services stanza landing this session), then a
claudia release so jevons and ytt can drop `replace`, then 🎯T63
(jevonsd restart on a clean tree against that tag), 🎯T62 (TLA+),
🎯T64 (Acquire/pool; blocked on T2.3). 🎯T65's plan-cache `-race`
flake is fixed on v0.33.0 (write-before-release + recheck under lease);
the target stays open pending vcheck.

Hazards learned: an installed daemon is reachable from every `go test`
on the machine — consumer hermetic suites must set
`CLAUDIA_NO_BROKER=1` (claudia, jevons and ytt now do); the launchd
service needs the shell environment (TERM, LANG, SHELL, …) or Claude's
TUI paints blank; the Claude plan window drains fast when the ytt
ingest runs four synopsis Tasks in parallel through the daemon
(HTTP 429 on the usage endpoint at 07:00Z), and Claude Session live
tests then fail on readiness — that is plan capacity, not the wire.
Codex live is blocked until 2026-09-15 by an exhausted ChatGPT plan.

# claudia broker — design notes

## Problem statement

claudia's warm pool (`pool.go`) is process-local. Each consumer process
maintains its own set of idle tmux windows, with no coordination across
processes on the same host. This creates three failure modes at
multi-consumer scale:

1. **No cross-process warm-pool sharing.** An idle agent in consumer A
   cannot serve consumer B. Every new process pays the cold-spawn
   latency (~600–700ms) even when capacity is sitting warm elsewhere.

2. **No global concurrency control.** N concurrent consumers can each
   spawn K agents, for N×K simultaneous `claude` processes. Anthropic
   rate-limit 429s are absorbed locally (by the pool's "spawn" fallback),
   but there is no host-level throttle to prevent the cascade.

3. **No shared observability.** Cost, active-agent count, per-model
   burn rate, and idle inventory are per-process. There is no single
   place to query "what is running, what has it cost, who spawned it".

A lifecycle broker daemon solves all three without changing the API that
consumers see today.

## Design decisions

### The broker owns lifecycle; the library becomes a thin RPC client

The broker is a lightweight Unix-socket daemon (`broker.sock`) that owns:

- The warm pool (a single shared inventory of idle agents across all
  consumers)
- Spawn admission (AIMD concurrency control, model-aware caps)
- Idle reaping (cost-weighted TTLs; opus reaps faster than haiku)
- Preemption (suspend lowest-priority idle session when a burst arrives)
- Spend tracking and rate-limit policy

The library (`Acquire`, `Release`, `NewTask`) remains the public API.
Internally it either talks to the broker over the socket or falls back
to direct-to-tmux if the socket is absent. The signature does not change.

### Library API is unchanged; an optional `Intent` hint is the only addition

No functional-options. No new required parameters. One optional field is
added to `Config`:

```go
// Intent hints at the expected interaction pattern. The broker uses it
// to assign a priority tier. Zero value is "auto", which infers from
// mode: Task → batch, Session with recent human input → interactive,
// idle Session → background.
Intent IntentHint // "auto" | "interactive" | "batch"
```

All policy knobs (caps, TTLs, preemption thresholds) live in the broker.
Callers declare intent, not policy.

### Auto-tuning policy, not knobs

The broker adapts all operational parameters automatically:

| Parameter | Mechanism |
|---|---|
| Concurrency caps | AIMD on observed 429 responses; start optimistic, back off on rate-limit, probe back up |
| Warm pool size | Rolling spawn rate per (model, workdir); drains when quiet |
| Idle reap TTL | Model-cost-weighted defaults; opus O(minutes), haiku O(hour) |
| Priority | Task=batch, Session with recent human input=interactive, idle Session=background |
| Preemption | Triggered by budget or concurrency pressure; suspends longest-idle lowest-tier first; auto-resumes |
| Spend smoothing | Rolling average; sudden 10× day raises a warning event but does not block |

The single user-facing knob is an optional global daily ceiling in
`~/.config/claudia/broker.yaml`:

```yaml
daily_cap_usd: 50.00  # optional; omit for no cap
```

Without it, the broker throttles only on 429s and model-tier policy.
With it, soft-throttle begins at 80% of the cap; hard-stop at 100%.

The rationale for minimising knobs: every additional parameter is a
vector for misconfiguration, and the autotuner's feedback loop produces
correct behaviour in the steady state. The only scenario that genuinely
requires a knob is runaway-spend protection, which has no correct
automatic default because "correct" is a budget policy.

### Observable, not controllable

Every broker decision (throttle, reap, preempt, resume) emits a
structured event on a `claudia broker tail` stream and into the audit log
at `~/.local/state/claudia/broker-audit.jsonl`. Power users see policy
in action; they do not configure it. If a decision is consistently wrong,
that is a bug in the policy, not a knob to expose.

Analogy: SQL Server's self-tuning memory manager. Smart defaults,
observable, one safety dial.

### Brokerless fallback

If `~/.local/state/claudia/broker.sock` is absent, the library behaves
exactly as it does today: process-local pool, direct tmux operations, no
concurrency coordination. This preserves single-consumer embedding use
cases and ensures the library continues to work without any daemon setup.

### Cost data sources

Two loops feed the spend tracker:

**Realtime (fast, exact for claudia-initiated work).**
`TaskEventResult.CostUSD` in Task mode; per-turn token counts from the
Session JSONL multiplied against a price table in Session mode. Same
source the ccusage community tool uses (`~/.claude/projects/*.jsonl`).
Latency: sub-second after each response.

**Reconciler (slow, ground truth).**
Anthropic Admin Cost API (`GET /v1/organizations/cost_report`) polled
every ~10 minutes to catch drift and workbench/shared-key usage that
bypasses the local tracker. Requires `ANTHROPIC_ADMIN_API_KEY` in the
broker's environment. If absent, broker logs "reconciler disabled, running
open-loop" and falls back to realtime-only tracking.

Forecasting for the daily ceiling: Holt-Winters with seasonality
(per-day-of-week seasonal component). Draws on cc-budget's approach;
more accurate than a rolling P95 because it accounts for weekly
periodicity in usage patterns.

For Session-mode cost, the broker reads the JSONL source rather than
maintaining its own price table — the ccusage project's pricing data is
more actively maintained and avoids the per-model-version drift problem.

## Prior art

| Tool | Type | Scope | Gap |
|---|---|---|---|
| ccusage (4.8k★) | CLI / read-only | Parses `~/.claude/projects/*.jsonl`; daily/weekly/session/5-hour-block views; offline | No control, no lifecycle, no cross-consumer coordination |
| Claude Code `/cost` | Built-in | Per-model breakdown, cache hit rates, rate-limit utilisation (v2.1.92+) | Per-session, no host-level view |
| Claude Code statusline JSONL | Built-in | Cumulative cost and rate-limit % snapshots | Read-only, no control |
| Workspace rate limits (Console) | Platform | Cap a workspace's API share | Org-level only, no per-host or per-model granularity |
| Bifrost (Maxim AI, Go) | Proxy | Sits in front of Anthropic API via `ANTHROPIC_BASE_URL`; ~11µs overhead; hierarchical budgets, virtual keys | Per-request gate; does not handle agent spawn/lifecycle/preemption |
| LiteLLM | Proxy | Vendor-neutral; spend-by-key tracking | Same as Bifrost |
| cc-budget | Monitor | Holt-Winters forecasting, seasonality-aware alerts | Read-only |

claudia's broker is **complementary** to Bifrost/LiteLLM: they gate at
the HTTP request layer; the broker gates at the agent lifecycle layer.
An operator could run both — Bifrost as the request-level enforcement
point, broker as the spawn/pool/preempt policy engine. The novel
value claudia adds is agent lifecycle policy, which no existing tool
handles.

## Anthropic Admin API endpoints

| Endpoint | Granularity | Auth | Freshness |
|---|---|---|---|
| `GET /v1/organizations/usage_report/messages` | 1m / 1h / 1d; group by model, workspace, API key, service tier | `sk-ant-admin...` (org accounts only) | ~5 min |
| `GET /v1/organizations/cost_report` | daily; group by workspace / description | same | ~5 min |
| Rate Limits API | live; configured limits per model | same | live |

No endpoint exposes a "credit remaining" or account-balance figure. The
broker works around this: real-time spend from local JSONL + Holt-Winters
forecast over rolling history = a derived daily ceiling that requires no
human-set dollar amount unless the user wants one.

For individual accounts (not org accounts), the Admin API is unavailable.
In that case the broker operates reconciler-disabled and the optional
`daily_cap_usd` dial becomes mandatory for spend protection rather than
optional.

## mnemo as cost backend

`mnemo_usage` already supports the dimensions the broker needs:
`group_by=day` (verified), `group_by=model` (verified), plus `days` and
`filter` parameters. The broker can query mnemo directly for historical
spend without maintaining its own database. mnemo's active-hours
normalisation (burn rate per hour of actual usage) is more informative
for policy decisions than raw daily totals.

This is preferable to a dedicated broker store for historical data because:
- mnemo is already indexing the same JSONL source
- cross-repo and cross-session aggregation comes for free
- mnemo's per-hour cost view directly informs the AIMD tuner

The broker's own write path is append-only events for audit and real-time
spend state; it does not need a full analytical store.

## Wire protocol

The broker listens on `~/.local/state/claudia/broker.sock` (Unix domain
stream socket). Each connection carries a session-scoped JSONL conversation
— a streaming connection per managed agent, not a request/response RPC.
This lets the broker push lifecycle events (preempted, resumed, reaped) to
the consumer without polling.

### Client → Broker messages

```jsonc
// Request a session or task agent. broker responds with "granted" or
// "queued"; consumer holds the connection open to receive further events.
{"type":"acquire","id":"req-1","model":"claude-opus-4-7","mode":"session|task","intent":"auto|interactive|batch","workdir":"/path/to/repo"}

// Heartbeat — sent ≤ every 5s by the consumer while the agent is in use.
// Absence for >30s causes the broker to reclaim the agent.
{"type":"heartbeat","session_id":"abc123"}

// Return the agent to the pool or request teardown.
{"type":"release","session_id":"abc123","disposition":"reuse|stop"}

// Subscribe to the broker event stream (broker → client, one-way push).
// Sent on a separate connection; broker streams all lifecycle events.
{"type":"tail"}

// Request a status snapshot.
{"type":"status"}
```

### Broker → Client messages

```jsonc
// Acquire response — agent is ready.
{"type":"granted","id":"req-1","session_id":"abc123","pid":78234,"warm":true}

// Acquire response — queued behind higher-priority or cap-limited work.
{"type":"queued","id":"req-1","position":2,"eta_ms":1400}

// Broker suspended the agent due to concurrency pressure or budget.
// Consumer should pause its own work; agent is SIGTSTP'd.
{"type":"paused","session_id":"abc123","reason":"concurrency_pressure|budget_soft_cap"}

// Broker resumed the agent (SIGCONT sent).
{"type":"resumed","session_id":"abc123"}

// Broker reaped the agent due to idle TTL or hard budget cap.
// Consumer should treat the session as gone.
{"type":"reaped","session_id":"abc123","reason":"idle_timeout|budget_hard_cap","idle_s":240}

// Tail-stream events (emitted on "tail" connections alongside above).
{"type":"spawn","session_id":"abc123","model":"claude-opus-4-7","pid":78234,"warm":true,"intent":"interactive"}
{"type":"429","model":"claude-opus-4-7","concurrency_cap_before":6,"concurrency_cap_after":4}
{"type":"cap_probe","model":"claude-opus-4-7","concurrency_cap":5}
{"type":"spend_warn","daily_usd":42.30,"cap_usd":50.00,"pct":84.6}

// Status snapshot response.
{"type":"status","active_agents":3,"warm_pool":2,"concurrency_caps":{"claude-opus-4-7":5,"claude-sonnet-4-6":8},"daily_usd":42.30,"cap_usd":50.00}
```

The acquire connection doubles as the event channel for that agent's
lifecycle: `paused`, `resumed`, and `reaped` arrive there, not on the tail
stream. The tail stream carries host-wide events for dashboards and
`claudia broker tail`.

### Library integration

`Acquire` opens a connection, sends `{"type":"acquire",...}`, and blocks
until `granted` (or returns an error on `reaped`/timeout). It then starts a
goroutine that feeds broker events to the agent's internal event loop:
`paused` suspends the consumer's `Send` path (queues outbound messages);
`resumed` drains the queue; `reaped` closes the agent with an error.

In brokerless mode, `Acquire` skips the socket entirely and falls through to
the existing `Start` path.

## Preemption mechanics

Preemption is SIGTSTP to the `claude` process (not the tmux window). The
broker owns the PID because it spawned the agent. SIGCONT resumes it.

Why SIGTSTP rather than tmux-level suspend:
- tmux `suspend-client` suspends the *user's* shell attachment, not the
  `claude` process itself — wrong target for programmatic throttling.
- `claude` process receives SIGTSTP, pauses its own I/O and compute, and
  resumes cleanly on SIGCONT. tmux continues to own the PTY; the window
  stays attached and observable.
- SIGTSTP is the natural Unix mechanism for scheduler-driven pause/resume;
  `kill -TSTP <pid>` is safe for any process that doesn't mask it.

Preemption policy:
1. Only Session agents are preemptable (Task agents are short-lived; killing
   them is cheaper than suspending).
2. Candidate selection: longest-idle, lowest-priority first. Ties broken by
   model cost (opus preempted before sonnet before haiku).
3. Before preempting, the broker sends `{"type":"paused",...}` to the
   consumer, then waits 200ms for any in-flight `Send` to drain, then sends
   SIGTSTP.
4. Resume on capacity recovery or budget headroom restoration; SIGCONT sent
   before `{"type":"resumed",...}` so the agent is live before the consumer
   queues new work.

## Open threads

- **mnemo group_by dimensions**: `group_by=day` and `group_by=model`
  verified. Whether `week`, `repo`, `session`, `block`, and `project` exist
  or need to be added is unconfirmed — check before relying on them in the
  broker's cost queries.
- **Price table for Session-mode realtime cost**: two options — (a) vendor
  a snapshot of ccusage's pricing data and update it at build time, or (b)
  skip the local price table entirely and rely on the Anthropic Cost API
  reconciler for per-session cost (accepts ~5-minute lag). Option (b) is
  simpler and removes maintenance burden; option (a) is lower-latency.
- **Daemon supervision packaging**: `brew services`-compatible launchd plist
  for macOS, systemd unit for Linux. Tracked as 🎯T2.7.
- **Per-session cost on `Event`**: Session mode currently does not emit cost
  on each event; adding it requires a price table or broker RPC. Tracked as
  a separate target — do not block broker implementation on it.
- **Acquire connection lifetime on heartbeat timeout**: 30s absence causes
  reclaim, but the right UX when the consumer crashes mid-session
  (vs. graceful release) needs a decision: auto-reuse, auto-stop, or
  configurable-per-intent.

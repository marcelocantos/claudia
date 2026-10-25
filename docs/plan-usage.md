# Plan usage (subscription remaining + rollover)

Status: product surface for 🎯T18 (2026-08-09).

This is **subscription-style plan remaining**, not per-run token
[`Usage`](../task.go) / `CostUSD` on Task events. Hosts (e.g. jevons)
call this to back off before exhausting plan windows.

## Broker observation pipeline

The broker's scheduled monitor owns vendor polling. It reserves each attempt
in its existing `claudia-control.db` before the request, then atomically writes
the completed outcome, last successful reading, successful history point and
next retry time. Successful readings are paced at five minutes; failed polls
retry automatically after one minute. HTTP 429/529 responses use the durable
15-minute doubling backoff, capped at two hours. Restarting the broker or
reloading a consumer cannot waive it. Requests time out after 30 seconds.

`claudia_plan_usage` has one row per provider. `status`, `reason` and
`attempted_at` describe polling; `success_at` and `reading_json` retain the
last successful observation across failures. `next_attempt_at` and `refusals`
are the durable schedule. A reservation preserves a prior completed outcome.
`claudia_plan_usage_history` retains successful readings indexed by provider
and observation time; a failure never creates a history point. Reading JSON
omits vendor raw bodies, preserves credits/spend, and enriches windows with
Claudia's `band` and finite `pressure` at the successful observation time.
Cursor publishes both total and API bucket windows.

The existing sqlpipe pair replicates both tables into the consumer's own
control database. `Replica.SubscribeQuery(ctx, query)` registers one SQL or
sqldeep query, initially returns persisted cached rows, and delivers complete
results on initial sync, reconnect and live changes. Local `Replica.Write`
commits reevaluate registered queries for local settings. It has no refresh
timer. Slow readers receive the latest result; cancellation or replica close
closes the result channel. Local tables outside the pair are excluded from
replication and schema negotiation.

Broker usage RPC/CLI reads are passive, including the legacy refresh flag.
They read current persisted outcomes and omit last-good windows from failed
providers so routing cannot mistake retained display values for fresh health.
The shared JSON readings/throttle files below belong only to the legacy direct
library APIs, not this broker pipeline. Broker authentication continues to
reuse the existing credential owner and noninteractive renewal path.

## API

```go
pu, err := claudia.QueryPlanUsage(ctx, &claudia.PlanUsageArgs{
    Provider: claudia.ProviderClaude, // or Codex / Grok / Bedrock / Cursor
})
// pu.Status: PlanUsageAvailable | PlanUsageUnavailable
// pu.Windows: []PlanWindow{ Name: session|weekly, UsedPercent, RemainingPercent, ResetsAt }

all, err := claudia.QueryAllPlanUsage(ctx, &claudia.AllPlanUsageArgs{})
```

Never invent numbers: when a backend does not publish remaining/rollover
(or credentials are missing), `Status == PlanUsageUnavailable`, `Windows`
is empty, and `Reason` explains why.

## Semantics by provider

| Provider | Status when signed in | Windows | Source |
| --- | --- | --- | --- |
| **Claude** | available (Pro/Max OAuth) | `session` ← five_hour, `weekly` ← seven_day | `GET https://api.anthropic.com/api/oauth/usage` with Claude Code OAuth token |
| **Codex** | available (ChatGPT login) | primary/secondary mapped by `limit_window_seconds` (~5h → session, ~7d → weekly) | `GET https://chatgpt.com/backend-api/wham/usage` with Codex `auth.json` tokens |
| **Grok** | available (signed in) | `weekly` ← SuperGrok pool | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits` with the `grok login` token — **undocumented, unversioned** ([grok-usage-billing.md](grok-usage-billing.md)) |
| **Bedrock** | **unavailable** | — | No Claude-style subscription remaining; AWS account quotas live in AWS |
| **Cursor** | available (signed in) | `weekly` ← billing-cycle `totalPercentUsed`, `api` ← `apiPercentUsed` | `POST https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage` — **undocumented, unversioned** |

### Claude

- Auth: macOS keychain service `Claude Code-credentials` →
  `claudeAiOauth.accessToken`, or env `CLAUDIA_CLAUDE_OAUTH_TOKEN`.
- Header: `anthropic-beta: oauth-2025-04-20`.
- Response fields: `five_hour.utilization` / `resets_at`,
  `seven_day.utilization` / `resets_at` (RFC3339).
- **Remaining** = `100 - utilization` (clamped 0–100).
- `session` is the product name for the 5-hour rolling window (Claude
  `/usage` “current session”); `weekly` is the 7-day window.
- API-key-only users without OAuth typically get unavailable (401 /
  missing token) — explicit reason, no fabricated %.

### Codex

- Auth: `~/.codex/auth.json` → `tokens.access_token` (+ optional
  `account_id` as `ChatGPT-Account-Id`), or env
  `CLAUDIA_CODEX_ACCESS_TOKEN` / `CLAUDIA_CODEX_ACCOUNT_ID`.
- Endpoint is ChatGPT product backend (`wham/usage`), not the public
  OpenAI platform billing API.
- `primary_window` / `secondary_window` may each be null. When OpenAI
  temporarily lifts the 5-hour window, only weekly may appear — that is
  reported as a single `weekly` window; session is **omitted**, not
  invented as 100%.
- `reset_at` is Unix seconds → `ResetsAt` UTC.
- `PlanType` carries `plan_type` when present (e.g. `"pro"`).

### Grok

Always fetched. The surface is the undocumented endpoint the CLI's own
`/usage` panel reads; it is private and unversioned. A break is
unavailable-with-reason — fix the parser, do not gate the read.

- Auth: the `grok login` OIDC token from `~/.grok/auth.json` (the long-lived
  `key` under the `auth.x.ai::…` entry), or `PlanUsageArgs.GrokAccessToken`.
  xAI issues a **six-hour** access token plus a refresh token, and the CLI
  rotates the pair only when it starts. A 401 therefore usually means the
  token expired with no grok session since — so the fetch rotates it
  itself (🎯T74): one headless `grok -p` turn in a throwaway directory,
  which rewrites `auth.json` the way the CLI does on start, then a reload
  and one retry. Rate-limited to one rotation per ten minutes per process;
  never for a non-401 failure. `PlanUsageArgs.GrokTokenRefresh` replaces
  the one-shot (tests, consumers), `GrokRefreshDisabled` turns it off. A
  401 that survives the rotation means the refresh token itself is gone:
  that is when `grok login` is needed.
- Endpoint: `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits`
  with `Authorization: Bearer <token>` and `X-XAI-Token-Auth: xai-grok-cli`.
- Only a **weekly** SuperGrok pool is published (no rolling session window).
  **Remaining** = `100 - config.creditUsagePercent` (`creditUsagePercent` is
  *used*, not remaining — verified against the panel "Weekly limit left: 0%"
  at `creditUsagePercent: 100`). `ResetsAt` ← `config.currentPeriod.end`.
- Fail-loud: a missing/out-of-range percent, a non-200, or an unparseable
  body all return unavailable with a reason — never a fabricated number.
- The `x.ai/billing` ACP extension is **pager-internal**, not exposed to
  external ACP clients (`grok agent stdio` returns "Method not found"), so the
  HTTP endpoint is the route rather than the ACP session.

### Cursor

Always fetched. The surface is the undocumented dashboard RPC the
Cursor billing UI reads; it is private and unversioned. A break is
unavailable-with-reason — fix the parser, do not gate the read.

- Auth: `PlanUsageArgs.CursorAccessToken`, then `CURSOR_API_KEY`, then
  the Cursor IDE session token: `ItemTable` key `cursorAuth/accessToken`
  in `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`
  (`~/.config/Cursor/…` on Linux; `PlanUsageArgs.CursorAuthPath`
  overrides). The read uses the SQLite linked into claudia (sqlpipe), not
  `sqlite3(1)`, and never opens the IDE's file in place: that SQLite opens
  read-write without URI support, and a read-write connection can
  checkpoint or delete the IDE's `-wal`. The file and its `-wal` are
  copied into a private temp directory, read, and removed. A missing file
  or key is unavailable with a reason naming the path. Plan usage is not a
  seat, so the legacy CLI-transport refusals (`af439b7`) do not apply.
- Only a **billing-cycle** included pool is published (mapped onto
  the longer `weekly` window with `LimitWindow` from
  `billingCycleStart`/`billingCycleEnd`). There is no rolling
  session window.
- **Remaining** = `100 - planUsage.totalPercentUsed`. A missing or
  out-of-range percent is unavailable — never a fabricated number.

### Bedrock

Token usage on Task events may still populate; **plan remaining** is
out of scope. AWS Service Quotas / Budgets are operator tooling, not
this API.

## Shared cache (🎯T61.2)

`LoadPlanUsage` is the multi-process path. Snapshots live under
`$CLAUDIA_PLAN_CACHE` or `UserCacheDir()/claudia/plan-usage/`. A fresh
snapshot (default TTL 5m) is returned without vendor calls. A miss takes
an exclusive lease (`lock.json` + flock); the holder heartbeats; a quiet
holder (default 20s) is stolen. The holder writes the snapshot before
releasing the lease and rechecks under the lock before fetching, so a
waiter that observed a miss does not call the vendor again. Unique tmp
names keep overlapping writers (lease steal) from clobbering. This is
the brokerless fallback — not a daemon.
When a broker is listening, the daemon is the one evaluator and this
cache is the degraded path (🎯T2.9; see [metaharness.md](metaharness.md)).

## Bands and Resolve (🎯T61)

`ClassifyPlan` / `ClassifyWindow` own the T596 pressure model. Hosts must
not keep a second copy of the vertices.

`HasAvailableTokens` is the automatic Resolve predicate: skip weekly-hot
and session-low/exhausted and 429 reasons; unpublished usage stays eligible.

`Resolve` picks a catalog `(Provider, Model)` from predicates. It does not
`Start` or `SwitchModel`. The catalog is a set: available-tokens
is a veto, then the survivors are ranked (below). `PreferProvider`
filters the pool to one provider before ranking; `TieBreak` biases
within reach of the leader. With `Purpose` (or wire `skill`) set it reads the 🎯T71 intel
series and also returns `Effort`. A purpose with no catalog-overlapping
observations is interpreted as `general` (`purpose_fallback_from` on the
pick). See [model-intel.md](model-intel.md).

### Catalog placement ranking (🎯T203)

Eligibility is unchanged and comes first: quality shelf, mode, plan
access, exclusions and owner keep-off overrides, `Background`,
`RequireUsage`, and available tokens (plan and model-scoped). An owner
dest-band override pins its provider. Otherwise the pool is the
dest-band rows (locked / under / ok), or, when no row is a published
dest, the unpublished rows. `PreferProvider` narrows the pool to its
provider when that provider is in it.

The pool is then ranked by these keys, in order:

| Key | Better | Why |
|---|---|---|
| Dest band | locked, under, ok | Surplus that expires unused goes first (🎯T693). |
| Weekly pressure | lower | The Spend Pressure Map's `ln(current pace / required pace)`. It is continuous, so readings 0.01 apart are ranked, not tied. |
| Weekly headroom | more percent left | The tighter of the plan window and a model-scoped window (🎯T86). |
| Session headroom | more percent left | Mostly separates rows whose weekly readings are identical. |

Before 🎯T203, weekly pressures within 0.05 of each other were
"undecided", and with no `TieBreak` Resolve returned `ErrTokenTied`.
Jevons never sends `TieBreak`, so a provider-less mint failed whenever
two plans were both eligible (2026-10-20, claude/claude-sonnet-5 vs
codex/gpt-6-sol). That dead band is gone. Its only remaining use is
`TieBreak`'s reach: the first `TieBreak` provider whose row has the
leader's band and pressure no more than 0.05 above the leader's wins
(`tie_break` in the pick's Reason). A preference can settle a gap that
small, which is noise in a reading the vendor rounds to whole percent.
It cannot settle a real pacing difference, cross a band, or reach an
ineligible row.

Rows equal on every key are drawn at random (`math/rand/v2`; the
`Rand` predicate pins the draw in tests), so mints spread evenly over
them. The draw is logged with the tied ids and the winner, and the pick's
Reason carries `drawn_of=N`. The common case is no usage published at
all, where every shelf row ties. `ErrTokenTied` is no longer returned;
the type stays for callers that match it. Resolve refuses only when no
row is eligible.

## Fleet roster and pick by remaining

The operator roster is exactly four providers, in this order: Cursor,
Grok (SuperGrok), Claude, Codex. `claudia broker usage -json` prints
that roster from the daemon's snapshot. Each row has `remaining_percent`
(the primary allowance: weekly, or the billing-cycle window mapped onto
weekly; the session window only when no longer one was published) and
`admit`, which is `HasAvailableTokens`. A missing reading is still a
row, with `remaining_percent` null. Numbers are never invented.

`claudia broker task --pick remaining` and `claudia broker grant --pick
remaining` ask the daemon to spawn the fullest admitted row: the
greatest primary remaining percent among rows `admit` would mark yes.
A known exhaustion (`plan_exhausted`) is skipped. A row with no
remaining percent cannot win. Equal percents break by roster order.
If none of the four admits with a number, the daemon returns
`plan_exhausted` and starts nothing. A grant of a name the daemon
already holds keeps that seat's provider.

The accounts are the ones already signed in on the host. This path
does not log in.

## Residual / honesty

- Endpoints used by Claude and Codex are **product backends** the
  official CLIs also call; they are not versioned OpenAPI contracts.
  Claudia maps the observed stable fields and treats HTTP/auth/schema
  failures as unavailable.
- Per-model scoped weekly limits (Claude `seven_day_opus` etc.) are not
  surfaced in v1 — only the primary session + weekly windows.
- Codex extra feature meters (`additional_rate_limits`) are not mapped
  in v1.
- Live network is not required for hermetic oracles; tests inject
  `HTTPClient` + token/URL overrides.

## Oracles

```bash
go test ./... -count=1 -run 'TestParseClaude|TestParseCodex|TestQueryPlanUsage|TestQueryAllPlanUsage|TestClassifyCodex|TestRemainingFromUsed|TestClassifyWindow|TestHasAvailable|TestLoadPlanUsage|TestResolve|TestFleetUsage|TestPickByRemaining'
```

## Related

- 🎯T14.4 Codex subscription usage (deeper throttle policy) may share this surface.
- Per-run tokens: `TaskEvent.Usage`, `Agent.Usage()` — different concern.

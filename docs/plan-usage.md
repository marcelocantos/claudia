# Plan usage (subscription remaining + rollover)

Status: product surface for 🎯T18 (2026-08-09).

This is **subscription-style plan remaining**, not per-run token
[`Usage`](../task.go) / `CostUSD` on Task events. Hosts (e.g. jevons)
call this to back off before exhausting plan windows.

## API

```go
pu, err := claudia.QueryPlanUsage(ctx, &claudia.PlanUsageArgs{
    Provider: claudia.ProviderClaude, // or Codex / Grok / Bedrock
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
| **Grok** | available **opt-in** (`CLAUDIA_GROK_USAGE=1`) | `weekly` ← SuperGrok pool | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits` with the `grok login` token — **undocumented, unversioned** ([grok-usage-billing.md](grok-usage-billing.md)) |
| **Bedrock** | **unavailable** | — | No Claude-style subscription remaining; AWS account quotas live in AWS |

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

**Opt-in** — off unless `CLAUDIA_GROK_USAGE=1` (or `PlanUsageArgs.GrokUnstableUsage`).
The surface is the undocumented endpoint the CLI's own `/usage` panel reads;
it is private and unversioned, so it is not on by default and may break on any
grok update.

- Auth: the `grok login` OIDC token from `~/.grok/auth.json` (the long-lived
  `key` under the `auth.x.ai::…` entry), or `PlanUsageArgs.GrokAccessToken`.
  grok owns refreshing it; an expired token yields 401 → unavailable.
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

### Bedrock

Token usage on Task events may still populate; **plan remaining** is
out of scope. AWS Service Quotas / Budgets are operator tooling, not
this API.

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
go test ./... -count=1 -run 'TestParseClaude|TestParseCodex|TestQueryPlanUsage|TestQueryAllPlanUsage|TestClassifyCodex|TestRemainingFromUsed'
```

## Related

- 🎯T14.4 Codex subscription usage (deeper throttle policy) may share this surface.
- Per-run tokens: `TaskEvent.Usage`, `Agent.Usage()` — different concern.

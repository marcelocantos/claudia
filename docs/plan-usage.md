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
| **Grok** | **unavailable** | — | SuperGrok weekly/session has **no documented public API** ([grok-usage-billing.md](grok-usage-billing.md)) |
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

Documented decision tree in [grok-usage-billing.md](grok-usage-billing.md):
do not scrape private `x.ai/billing` ACP methods or call `grok -p "/usage"`.
Until xAI ships a stable usage API, this surface returns unavailable.

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

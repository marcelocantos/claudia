# Grok usage and billing surfaces

Status: research notes (2026-08-09). Not a product commitment and not a
public API contract. Captures what the Grok Build CLI and xAI docs expose
so claudia hosts do not invent SuperGrok balance features on unstable
surfaces.

Related:

- Session wire: [grok-acp-session.md](grok-acp-session.md)
- Provider oracles: [grok-provider-oracle-map.md](grok-provider-oracle-map.md)
- Host guide cost matrix: [agents-guide.md](../agents-guide.md)

## Why this note exists

claudia drives the **Grok Build CLI** (`ProviderGrok`) for Task and
Session. Hosts (e.g. jevons) sometimes want:

1. Per-run token / dollar spend for a prompt or session
2. SuperGrok **weekly usage %**, next reset, Extra Usage Credits, Auto Top Up
   (what the interactive `/usage` panel shows)
3. Console **API team prepaid** balance (xAI Console billing)

These are three different systems. Only (1) is partially available on
public CLI streams claudia already uses. (2) has **no documented API**.
(3) has a documented Management API, separate from SuperGrok product billing.

## Interactive `/usage` (TUI only)

In the Grok Build TUI:

```text
/usage
/usage manage   # opens billing management
```

Alias: `/cost`. Documented in the CLI user guide under Account and Billing
(`~/.grok/docs/user-guide/04-slash-commands.md`).

Typical panel fields (SuperGrok consumer billing):

| Field | Meaning |
| --- | --- |
| Session usage | Tokens / calls for this TUI session (or “no model calls yet”) |
| Weekly limit | Shared weekly usage pool consumption (percentage) |
| Next reset | When the weekly pool resets |
| Credits | Extra Usage Credits balance (pay-as-you-go after the pool) |
| Auto topup | Whether Auto Top Up is enabled (and caps when present) |

Product behaviour for weekly limits / Extra Credits is described in the
consumer FAQ: [docs.x.ai/grok/faq](https://docs.x.ai/grok/faq). UI also
appears at Settings → Usage and `https://grok.com/?_s=usage`.

## Headless: `grok -p` does not run `/usage`

`-p` / `--single` is headless single-turn mode. The argument is a **model
prompt**, not the slash-command router.

```bash
# Does NOT open the /usage panel — the model sees the literal text "/usage"
grok -p "/usage"
```

Reproduced 2026-08-09: the agent starts answering about usage, then hits
turn limits; it never renders the weekly-limit / credits panel.

There is no `grok usage` subcommand. Headless companions that *do* work:

| Goal | Command surface |
| --- | --- |
| Per-run tokens / cost metadata | `grok -p "…" --output-format json` → `usage`, `modelUsage`, cost fields when present |
| Streaming agent updates | `--output-format streaming-json` (what claudia Task uses) |
| Messages-shaped stream | `--output-format streaming-messages-json` |

claudia Task mode uses `streaming-json`. That stream maps text and terminal
`end` / `error`; **tool-use and cost/usage are not on that public stream**
(see agents-guide capability matrix). Prefer headless `json` result fields
when a host needs spend automation outside claudia’s Task event model.

## Two product billing systems

| System | Who | What | Programmatic access |
| --- | --- | --- | --- |
| **SuperGrok consumer** | grok.com / apps / Build subscription | Shared weekly pool, Extra Usage Credits, Auto Top Up | **None documented** |
| **API team prepaid** | [console.x.ai](https://console.x.ai) teams | Prepaid credits, invoices, spending limits, usage analytics | **Management API** (documented) |

`/usage` in Build is the SuperGrok consumer view (plus session counters).
Console prepaid balance is a different ledger.

## How `/usage` is wired (internal)

Slash commands are split between shell builtins (`xai-grok-shell`) and
pager builtins (`xai-grok-pager`). `/usage` is not a model skill; the TUI
loads data via **ACP extension methods** and private upstream HTTP.

### ACP extensions (pager ↔ agent)

Observed method names in the Grok binary (not in public docs, not in
`~/.grok/docs`):

| Method | Role (inferred) |
| --- | --- |
| `x.ai/session/usage` | Session-level token / call ledger for the open agent session |
| `x.ai/billing` | Billing config / credits config for the panel |
| `x.ai/auto-topup-rule` | Auto Top Up rule (enabled, thresholds, caps) |
| `x.ai/auth/check_subscription` | Subscription / tier checks (related auth path) |

These are JSON-RPC-style extensions on the local agent channel (same family
as other `x.ai/*` methods). They are **not** REST paths on
`https://api.x.ai`. claudia’s Session mode speaks public ACP methods only
and ignores private `_x.ai/*` / product extensions unless explicitly
added later.

### Upstream HTTP (agent ↔ xAI product backends)

From binary strings (paths and errors; **undocumented**):

- Path fragment: `/billing?format=credits`
- Path fragment: `/auto-topup-rule`
- Auth: Bearer token from **`grok login`** OIDC (`~/.grok/auth.json` →
  `auth.x.ai`); error copy says billing data “requires auth with grok.com”
- Headers observed in string tables: `Authorization: Bearer …`,
  `x-grok-client-mode`, client version / identifier family
- Proxy host in CLI config defaults: `https://cli-chat-proxy.grok.com/v1`
  (inference + settings; enterprise allowlist docs list this host)

Exact base URL, schema, and stability are **not published**. Suitable for
forensics only — not for claudia or host automation.

Response-shaped field names seen in the binary for billing structs
include (names only; no schema guarantee):
`prepaidBalance`, `creditUsagePercent`, `includedUsed`, `totalUsed`,
`billingPeriodStart` / `billingCycle`, `onDemandCap` / `onDemandUsed`,
`isUnifiedBillingUser`, top-up fields (`topupAmount`, `maxAmountPerMonth`,
`minBeforeHittingSl`), plus UI labels `Weekly limit`, `Next reset`,
`Credits`, `Auto topup`.

## Documented public API (API team only)

Base URL: `https://management-api.x.ai`  
Auth: [management key](https://console.x.ai/team/default/management-keys)  
Reference: [Billing Management](https://docs.x.ai/developers/rest-api-reference/management/billing)

Useful routes:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/billing/teams/{team_id}/prepaid/balance` | Prepaid balance + change history |
| POST | `/v1/billing/teams/{team_id}/prepaid/top-up` | Manual prepaid top-up |
| POST | `/v1/billing/teams/{team_id}/usage` | Historical usage analytics |
| GET | `/v1/billing/teams/{team_id}/postpaid/spending-limits` | Soft/hard spending limits |
| GET | `/v1/billing/teams/{team_id}/postpaid/invoice/preview` | Current postpaid preview |
| GET | `/v1/billing/teams/{team_id}/invoices` | Invoices |

Example:

```bash
curl -sS -H "Authorization: Bearer $XAI_MANAGEMENT_KEY" \
  "https://management-api.x.ai/v1/billing/teams/$TEAM_ID/prepaid/balance"
```

Amounts in this API are typically **USD cents** objects (`{ "val": "…" }`).
This does **not** expose SuperGrok weekly limit percentage or consumer
Auto Top Up state.

Console product docs for prepaid / auto top-up (human UI):  
[docs.x.ai/console/billing](https://docs.x.ai/console/billing).

## Implications for claudia

| Host need | Recommendation |
| --- | --- |
| Per-Task text result | `ProviderGrok` Task + `streaming-json` (today) |
| Per-Task dollar / full usage parity with Claude | **Not available** on streaming-json; do not fake from reverse-engineered billing |
| SuperGrok weekly % / credits / Auto Top Up | **Unavailable** via `QueryPlanUsage(ProviderGrok)` until xAI documents a stable API; use TUI `/usage` or grok.com UI. Multi-provider surface: [plan-usage.md](plan-usage.md) |
| API team prepaid balance | Host-side Management API client (management key); not via `grok -p` or ACP Session |
| Scraping `/billing?format=credits` with user OAuth | Reject for product code: private, undocumented, will break |

Do **not** teach claudia to:

1. Treat `grok -p "/usage"` as a balance oracle
2. Call private `x.ai/billing` / `x.ai/auto-topup-rule` over ACP without a
   versioned, documented contract
3. Drive Session mode by parsing `~/.grok` private storage for billing

If a future Grok CLI ships `grok usage --json` or documented product
billing endpoints, re-evaluate under a dedicated target and oracle map.

## Quick decision tree

```text
Need spend for this run?
  → headless json result usage/cost when available
  → claudia Task streaming-json: text + session id only (no cost)

Need SuperGrok weekly / Extra Credits / Auto Top Up?
  → TUI /usage or grok.com Settings → Usage
  → no public API as of 2026-08-09

Need console API prepaid balance / team invoices?
  → Management API (management-api.x.ai) with management key
```

## Sources (2026-08-09)

- Grok CLI user guide: `~/.grok/docs/user-guide/04-slash-commands.md` (`/usage`),
  `14-headless-mode.md` (`-p`, output formats)
- Grok binary string tables (`~/.grok/bin/grok`): ACP method names, UI copy,
  `/billing?format=credits`, `/auto-topup-rule`, billing field names
- Live repro: `grok -p "/usage"` treated as model prompt, not slash command
- xAI docs: Management billing API, console billing, SuperGrok FAQ
- Enterprise allowlist mentions `cli-chat-proxy.grok.com` (inference / settings)

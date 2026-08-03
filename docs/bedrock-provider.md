# AWS Bedrock Provider Design (🎯T12 / T12.1)

Status: design for `ProviderBedrock` — **API-based** Anthropic Claude on
AWS Bedrock for a work account. Not Claude Code CLI.

## Goal

Reach Anthropic Claude models through **AWS Bedrock ConverseStream**
without a local `claude` binary:

1. Configure region, model ID, and credential source (no secrets in git).
2. Send a user prompt.
3. Receive **streamed assistant text** as `TaskEventText` chunks, then a
   terminal `TaskEventResult` (or `TaskEventError`).

This is owner mission element 2. Element 1 (`ProviderClaude` CLI path)
remains separate (🎯T11); Bedrock must not break Claude/Codex/Grok
contracts.

## Placement in claudia

| Surface | Bedrock v1 |
| --- | --- |
| `Provider` constant | `ProviderBedrock` (`"bedrock"`) |
| Task mode | **Supported** — `bedrockTaskBackend` via `taskBackendForProvider` |
| Session mode | **Unsupported** — fail-closed `CapabilityError` (no tmux / JSONL) |
| Registry / Pool | Task-only hosts can use Bedrock; Session Launch fails closed |
| Package `claudia/grok` | Unrelated (Realtime voice) |

Seams stay **additive**: new switch cases and a new backend type. Empty
`Provider` continues to mean `ProviderClaude`.

### Why Task, not a free-standing client only

Jevons and other consumers already select `claudia.Provider` + `NewTask` /
`Task.Run`. Shipping Bedrock as a Task backend reuses event types,
cancel/stop, and hermetic spawn patterns without inventing a second
host contract. A thin internal streamer interface keeps AWS out of unit
tests.

## Transport and API

- **API:** Bedrock Runtime **ConverseStream** (not InvokeModel, not the
  Claude Messages HTTP API directly).
- **SDK:** AWS SDK for Go v2 (`config` + `service/bedrockruntime`).
- **Auth:** Default credential chain (env keys, shared config/credentials,
  SSO, ECS/EC2/IRSA roles, etc.). Optional profile via `AWS_PROFILE` /
  explicit load option.
- **No secrets in git.** No committed access keys, session tokens, or
  account IDs required for hermetic tests.

Streaming model:

| Bedrock stream event | claudia `TaskEvent` |
| --- | --- |
| content block text delta | `TaskEventText` (chunk) |
| message stop (+ optional metadata usage) | `TaskEventResult` (full text + `Usage` when present) |
| service / validation / auth errors | `TaskEventError` or `Run` error before stream |
| tool use / images / document blocks | **Deferred** — not claimed in v1 |

`CostUSD` is not filled (Bedrock does not return USD cost on the stream).
Token `Usage` may be set from stream metadata when the service provides it.

Session id / resume: **not claimed**. Each `Run` is a single-turn
user→assistant ConverseStream. Multi-turn history, tools, and
cross-run resume are deferred.

## Config surface (no new TOML)

Prefer env + existing struct fields. No new TOML config format.

| Input | Source | Notes |
| --- | --- | --- |
| Provider select | `TaskConfig.Provider = ProviderBedrock` | Required to leave Claude default |
| Model ID | `TaskConfig.Model` or `CLAUDIA_BEDROCK_MODEL_ID` | Bedrock model id or inference profile id/ARN; empty is an error |
| Region | `CLAUDIA_BEDROCK_REGION`, else `AWS_REGION`, else `AWS_DEFAULT_REGION` | Required for client construction |
| Profile | `AWS_PROFILE` (standard) | Optional; default chain if unset |
| Credentials | AWS SDK default chain | Document work-account setup; never commit keys |
| Live smoke gate | `CLAUDIA_BEDROCK_LIVE=1` | Opt-in real AWS; not hermetic evidence |

Optional future (not required for v1 acceptance): endpoint override for
VPC/private endpoints via env only if needed; still no TOML.

`WorkDir`, `DisallowTools`, sandbox/approval fields are **ignored** for
Bedrock v1 (no local tools / sandbox). Callers that need tool denial
should not assume CLI semantics on this provider.

## Capability matrix vs ProviderClaude

| Capability | Claude CLI | Bedrock v1 | Residual |
| --- | --- | --- | --- |
| Task (prompt → events) | yes | **yes** (streamed text) | Live model access / IAM residual |
| Streamed assistant text | yes | **yes** | — |
| Tool use events | yes | **no** | Deferred; fail-closed if needed later |
| Session (tmux / persistent) | yes | **no** | Unsupported |
| Resume (`ClaudeID` / `--resume`) | yes | **no** | Stateless single-turn |
| Rewind | yes | **no** | Unsupported |
| Cost USD | yes (Task) | **no** | Tokens optional via Usage only |
| Permissions / disallow tools | yes | **n/a** | No tools in v1 |
| Tmux attach / terminal bytes | yes | **no** | N/A for pure API |
| Local `claude` binary | required | **not required** | Needs AWS creds + model access |

**Do not claim CLI parity.** Docs and capability structs must stay
honest: Task + streamed text only.

## Error mapping (hermetic)

| Condition | Behaviour |
| --- | --- |
| Unknown / empty model | `Run` error before network |
| Missing region | `Run` error before network |
| Credential / config load failure | `Run` error (no silent skip) |
| HTTP/API error mid-stream | `TaskEventError` and/or stream close with error path |
| Context cancel / Task.Cancel | Interrupt cancels stream context |

## Oracle plan (summary)

Full map: [bedrock-provider-oracle-map.md](./bedrock-provider-oracle-map.md).

- Hermetic: config resolution, request message shape, stream→`TaskEvent`
  mapping, auth-missing/error mapping, backend dispatch, Session fail-closed.
- Live residual: `CLAUDIA_BEDROCK_LIVE=1` smoke only; never sole
  achievement evidence.

## Work-account setup (summary)

See [bedrock-work-account.md](./bedrock-work-account.md) for IAM model
access, profile/region, and example model IDs. Hermetic CI needs no AWS.

## Non-goals (v1)

- Anthropic direct API (non-Bedrock).
- Claude Code tool loop, MCP, permissions, or tmux Session.
- New TOML config schema.
- Secrets or long-lived keys in the repository.
- Jevons consumer wiring (list for overseer → jevons-po if needed).

## Implementation sketch

1. `ProviderBedrock` in `provider.go`; `bedrockProviderCapabilities()`.
2. `bedrockTaskBackend` implementing `taskBackend`; injectible streamer
   for hermetic Run tests.
3. Production streamer: LoadDefaultConfig + ConverseStream.
4. Agent/Session/Rewind: unsupported `CapabilityError` for Bedrock.
5. Docs: this file, oracle map, work-account setup, README/agents-guide
   support matrix rows.

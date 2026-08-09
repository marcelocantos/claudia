# Codex App-Server Spike

Status: **proven for 🎯T4.4** (2026-08-09). Live stdio turn + resume/fork/archive/interrupt
captured against `codex-cli 0.146.0-alpha.9.2`. Hermetic oracles parse redacted fixtures;
production Session mode (🎯T4.5) may now depend on this public contract (not private files).

## Public Contract

Source: OpenAI Codex app-server docs + installed-version schema + live captures.

Codex app-server is the documented rich-client integration point for persistent Codex
conversations. Default transport is JSON-RPC-style JSONL over stdio:

- Requests have `method`, `id`, and `params`.
- Responses echo `id` with `result` or `error`.
- Notifications omit `id` and have `method` plus `params`.
- Client must send `initialize`, then `initialized`, before other methods.

Minimum handshake (request builders in `codex_app_server.go`):

```jsonl
{"method":"initialize","id":0,"params":{"clientInfo":{"name":"claudia","title":"claudia","version":"0.1.0"}}}
{"method":"initialized","params":{}}
{"method":"thread/start","id":1,"params":{"model":"gpt-5.4","cwd":"<WORKDIR>","approvalPolicy":"never","sandbox":"read-only"}}
{"method":"turn/start","id":2,"params":{"threadId":"<thread-id>","input":[{"type":"text","text":"<prompt>"}],"approvalPolicy":"never"}}
```

Documented / observed notification families include `thread/started`,
`thread/status/changed`, `turn/started`, `item/started`, `item/completed`,
`item/agentMessage/delta`, `thread/tokenUsage/updated`, `account/rateLimits/updated`,
and `turn/completed`.

## Live Capture (2026-08-09)

Binary: `/Applications/ChatGPT.app/Contents/Resources/codex` (`codex-cli 0.146.0-alpha.9.2`).
Auth: `~/.codex/auth.json` with `auth_mode=chatgpt` (subscription; `OPENAI_API_KEY` unset).

### Ephemeral turn (notification sequence)

`thread/start` with `ephemeral: true`, `approvalPolicy: "never"`, `sandbox: "read-only"`,
then `turn/start` with text `Reply with exactly: T4.4-ok`.

Observed order (MCP noise omitted):

1. `initialize` result (userAgent, codexHome, platform)
2. `thread/start` result — `model: gpt-5.4`, `approvalPolicy: never`,
   `sandbox: {type: readOnly, networkAccess: false}` (**request `sandbox: "read-only"`
   maps to response `type: "readOnly"`**)
3. `thread/started`
4. `turn/start` result — turn id, `status: inProgress`
5. `thread/status/changed` → active
6. `turn/started`
7. `item/started` / `item/completed` for `userMessage`, `reasoning`, `agentMessage`
8. `item/agentMessage/delta` stream
9. `thread/tokenUsage/updated` (usage lives here, **not** on `turn/completed`)
10. `thread/status/changed` → idle
11. `turn/completed` with `status: completed` and summary items

Redacted fixture: `testdata/codex/app-server/live-turn.jsonl`.

### Non-ephemeral lifecycle

Without `ephemeral`, the same host proved:

| Method | Result |
| --- | --- |
| `thread/resume` | ok — returns thread + model + approval + sandbox |
| `thread/fork` | ok — new thread id |
| `thread/archive` | ok — emits `thread/archived` |
| `thread/unarchive` | ok — emits `thread/unarchived` |
| `turn/interrupt` | ok — `turn/completed` with `status: interrupted` |

Redacted shapes: `testdata/codex/app-server/lifecycle.jsonl`.

**Ephemeral caveat:** `ephemeral: true` threads have no rollout path.
`thread/resume`, `thread/fork`, `thread/archive` then fail with
`no rollout found for thread id …`. Session mode must use non-ephemeral
threads when resume/fork/archive are required.

## Configuration Mapping (proven)

| Concern | Public field | Notes |
| --- | --- | --- |
| Working directory | `cwd` on `thread/start`, `turn/start`, `thread/resume` | Absolute path |
| Model | `model` | Resolved model echoed on `thread/start` / `thread/resume` result |
| Approval | `approvalPolicy` (`never`, `on-request`, …) | Live: request `never` → response `never` |
| Sandbox | **`sandbox`** on `thread/start` (string mode e.g. `read-only`) | Response object `{type:"readOnly",…}`. Do **not** send legacy `sandboxPolicy` alone on thread/start for this CLI. `permissions` cannot combine with `sandbox`. |
| Resume | `thread/resume` + `threadId` | Prefer threadId over unstable path |
| Fork | `thread/fork` + `threadId` | `path` marked unstable |
| Archive | `thread/archive` / `thread/unarchive` + `threadId` | |
| Interrupt | `turn/interrupt` + `threadId` + `turnId` | |

## Item type naming

Live 0.146+ uses camelCase item types (`agentMessage`, `commandExecution`,
`userMessage`). Hermetic fixtures may use snake_case. Parser normalizes via
`normalizeCodexAppServerItemType`.

## Schema Artifact Decision

```bash
codex app-server generate-ts --out ./schemas
codex app-server generate-json-schema --out ./schemas
```

Generated JSON Schema with `--experimental` (2026-08-09 recheck): ~4.2 MB
bundle under a temp dir. No repo-local license header; churn is version-tied.

**Decision (unchanged):** do not commit the generated schema bundle. Prefer
small hand-written / redacted live golden fixtures under
`testdata/codex/app-server/`. Regenerate schema to a temp directory when field
names need re-checking.

## Maturity Risk

- **stdio JSONL** — proven through full turn + lifecycle on 0.146.0-alpha.9.2.
- **WebSocket transport** — experimental / unsupported for claudia.
- **Experimental methods/fields** — require `capabilities.experimentalApi = true`;
  fixture `unsupported-capability.jsonl` + typed capability errors.
- **MCP fan-out noise** — thread/start may emit many
  `mcpServer/startupStatus/updated` notifications from the user's Codex config;
  Session clients must ignore unknown methods.
- **Usage accounting** — live path uses `thread/tokenUsage/updated` with camelCase
  token fields; hermetic success fixture also places snake_case usage on
  `turn/completed` for simplified lifecycle tests.
- **Alpha CLI** — interface can still change; pin fixtures and fail closed on
  missing methods.

**Fallback:** Codex Task mode via `codex exec --json` remains available.
ProviderCodex Session mode reports typed experimental/unsupported capability
errors rather than scraping private Codex session files or driving the TUI.

## Private storage

Production code must not read `.codex/sessions`, rollouts, or history paths.
Oracle: `TestCodexProviderDoesNotReadPrivateStorage`.

## Hermetic oracles

| Test | Covers |
| --- | --- |
| `TestParseCodexAppServerSuccessFixture` | Hand-owned success stream |
| `TestParseCodexAppServerLiveTurnFixture` | Live order + camelCase + tokenUsage + sandbox map |
| `TestParseCodexAppServerLifecycleFixture` | Resume/fork/archive/interrupt shapes |
| `TestParseCodexAppServerFailureFixture` | Failed turn |
| `TestParseCodexAppServerInterruptedFixture` | Interrupted turn |
| `TestParseCodexAppServerUnsupportedCapabilityFixture` | Experimental gate error |
| `TestCodexAppServerRequestBuilders` | Public request field names |
| `TestFakeCodexAppServerLifecycle` | Agent seam without real Codex |
| `TestCodexProviderDoesNotReadPrivateStorage` | No private-path shortcut |

## Residual (class-3 / follow-ups)

- Whether app-server maturity is **acceptable for product UX** is a human
  accept/reject (oracle map human residue), not a machine gate.
- Gated live Session smoke after 🎯T4.5 production wiring.
- Host MCP config noise is environment-specific; not part of the golden path.

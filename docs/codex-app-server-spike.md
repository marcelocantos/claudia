# Codex App-Server Spike

Status: converging for 🎯T4.4. No production Session-mode code may depend on this until the live notification sequence is captured and reviewed.

## Public Contract

Source: current OpenAI Codex manual, "Codex App Server", fetched 2026-07-06.

Codex app-server is the documented rich-client integration point for persistent Codex conversations. It uses JSON-RPC-style JSONL over stdio by default:

- Requests have `method`, `id`, and `params`.
- Responses echo `id` with `result` or `error`.
- Notifications omit `id` and have `method` plus `params`.
- The client must send `initialize`, then `initialized`, before other methods.

Minimum handshake:

```jsonl
{"method":"initialize","id":0,"params":{"clientInfo":{"name":"claudia","title":"claudia","version":"0.1.0"}}}
{"method":"initialized","params":{}}
{"method":"thread/start","id":1,"params":{"model":"gpt-5.4"}}
```

After `thread/start` returns a thread id, a turn starts with:

```jsonl
{"method":"turn/start","id":2,"params":{"threadId":"<thread-id>","input":[{"type":"text","text":"<prompt>"}]}}
```

Documented notification families include `turn/started`, `item/started`, `item/completed`, `item/agentMessage/delta`, tool progress, and `turn/completed`.

## Configuration Mapping To Prove

Generated schema inspection on 2026-07-06 confirmed these installed-version field names without starting a model turn:

- `thread/start` params include `cwd`, `model`, `approvalPolicy`, `permissions`, and config/developer-instruction overrides.
- `turn/start` params require `threadId` and `input`; optional turn overrides include `cwd`, `model`, `approvalPolicy`, `permissions`, and `sandboxPolicy`.
- `thread/resume` params require `threadId` and also include `cwd`, `model`, and `approvalPolicy`.
- `thread/fork` params require `threadId`; `path` is marked unstable and should not be preferred.
- `thread/archive` and `thread/unarchive` take `threadId`.
- `turn/interrupt` requires both `threadId` and `turnId`.

The live spike must still prove the response and notification sequence, including the turn id needed for interruption.

## Schema Artifact Decision

The installed CLI can generate TypeScript bindings and JSON Schema:

```bash
codex app-server generate-ts --out ./schemas
codex app-server generate-json-schema --out ./schemas
```

Generated JSON Schema with `--experimental` on 2026-07-06 produced a 4.0 MB bundle with 123,371 total lines and many version-specific v2 files. The aggregate schema files alone were ~21k and ~23k lines. The files did not include an obvious repo-local license/header in the inspected snippets, and their churn profile is intentionally tied to the installed Codex version.

Decision: do not commit the generated schema bundle. Prefer small hand-written golden fixtures for claudia tests, and regenerate the schema in a temp directory during future spikes when exact field names need re-checking.

## Maturity Risk

The app-server page describes the interface as the rich-client integration point, but several transports and some methods/fields are explicitly experimental:

- stdio JSONL is the safest transport for the first claudia integration.
- WebSocket transport is experimental and unsupported.
- Some app-server methods and fields require `capabilities.experimentalApi = true`.
- Persistent Codex Session mode must expose capability errors if a required method or field is unavailable.

Fallback if app-server changes: Codex Task mode remains available through `codex exec --json`; Session mode for ProviderCodex should report a typed unsupported/experimental capability error rather than scraping private Codex session files or driving the TUI.

## Live Capture Still Required

To finish 🎯T4.4, run a live app-server stdio client against the installed Codex binary, with explicit user approval because `turn/start` contacts the model. The capture should record:

- initialize response
- thread/start response
- turn/start response
- notification sequence through turn/completed
- exact fields for cwd/model/sandbox/approval/resume/fork/archive

The resulting redacted JSONL should live under `testdata/codex/app-server/` if small and stable, or be summarized here with a rationale if the raw fixture is too volatile.

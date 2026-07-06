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

The manual says `turn/start` can override model, personality, `cwd`, sandbox policy, and more. The live spike must confirm the exact installed-version field names for:

- `cwd`
- `model`
- sandbox policy
- approval policy
- resume via `thread/resume`
- fork via `thread/fork`
- archive/unarchive via thread APIs

Until the generated schema or a live exchange confirms those field names, claudia must not expose Codex Session mode as production-ready.

## Schema Artifact Decision

The installed CLI can generate TypeScript bindings and JSON Schema:

```bash
codex app-server generate-ts --out ./schemas
codex app-server generate-json-schema --out ./schemas
```

These artifacts are version-specific and likely high churn. Before committing generated files, inspect their license/header and size. Prefer small hand-written golden fixtures if the generated output is large, lacks an acceptable license notice, or changes frequently across Codex releases.

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

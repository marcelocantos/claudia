# Codex subscription headless spike (🎯T14.1)

Status: load-bearing unknown **resolved** (2026-08-09). Recorded before
further T14 feature work, same discipline as T5 rewind spikes.

## Question

Can `codex exec` run non-interactively on the cached ChatGPT **subscription**
OAuth token (not OpenAI API-key / per-token billing) and emit
machine-parseable structured output?

## Environment (this host)

| Item | Value |
| --- | --- |
| Binary | `/Applications/ChatGPT.app/Contents/Resources/codex` |
| CLI version | `codex-cli 0.146.0-alpha.9.2` |
| PATH | `codex` **not** on `$PATH` (resolver must use app-bundle fallback) |
| Legacy bundle | `/Applications/Codex.app/.../codex` **absent** (post desktop merger) |
| `~/.codex/auth.json` | `auth_mode=chatgpt`, non-empty `tokens.access_token`, `OPENAI_API_KEY` null |
| Env | `OPENAI_API_KEY` unset for the spike |

## Structured-output mode chosen

**`codex exec --json`** — prints **JSONL events on stdout**.

Documented by the installed CLI (`--json` → "Print events to stdout as JSONL").
This is the mode claudia Task mode already targets (`codexTaskArgs` appends
`--json`).

## Command shape that works

Global options belong **before** the `exec` subcommand; exec-specific options
(`--json`, `--ephemeral`, `--skip-git-repo-check`) belong after:

```bash
CODEX=/Applications/ChatGPT.app/Contents/Resources/codex
env -u OPENAI_API_KEY "$CODEX" \
  --sandbox read-only \
  --ask-for-approval never \
  exec \
  --json \
  --skip-git-repo-check \
  --ephemeral \
  'Reply with exactly: T14.1-ok'
```

Putting `--ask-for-approval` *after* `exec` fails on this CLI version
(`unexpected argument '--ask-for-approval'`). Production `codexTaskArgs`
already places approval/sandbox/model **before** `exec` — correct for
0.146.0-alpha.9.2.

## Observed JSONL (redacted)

Successful run (exit 0), four stdout lines:

```jsonl
{"type":"thread.started","thread_id":"<uuid>"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"T14.1-ok"}}
{"type":"turn.completed","usage":{"input_tokens":20520,"cached_input_tokens":9984,"cache_write_input_tokens":0,"output_tokens":9,"reasoning_output_tokens":0}}
```

Notes:

- Event type names match claudia's hermetic fixtures under
  `testdata/codex/exec/` (`thread.started`, `turn.started`,
  `item.completed` agent_message, `turn.completed` with token usage).
- `turn.completed.usage` reports **token counts only** — no USD cost field
  (consistent with subscription / plan allowance, not API invoice lines).
- No auth error, no prompt to open a browser, no API-key prompt when
  `OPENAI_API_KEY` is unset and `auth_mode=chatgpt`.

## Auth preflight (product)

claudia now exposes `PreflightCodexAuth` / `ensureCodexSubscriptionAuth`:

- Reads `~/.codex/auth.json` (or `CLAUDIA_CODEX_AUTH_PATH`).
- Treats `auth_mode=chatgpt` (or inferred access token) + non-empty
  `tokens.access_token` as **subscription OK**.
- Fails closed when mode is API-key, auth missing, or `OPENAI_API_KEY` is
  set in the environment (loud warning: per-token fall-through risk).
- `codexTaskBackend.RunTask` calls this before spawn.

Binary resolution (`resolveCodexBin`) order:

1. `CODEX_BIN`
2. `exec.LookPath("codex")`
3. candidates including **`/Applications/ChatGPT.app/Contents/Resources/codex`**
   then legacy `/Applications/Codex.app/Contents/Resources/codex`

## Residual

- Live `CLAUDIA_CODEX_LIVE=1` Task smoke remains the regression gate for the
  full claudia event mapping (`TestCodexTaskRunSmoke`).
- Whether ChatGPT plan rate windows are exhausted is not proven by a single
  ok turn — use `QueryPlanUsage(ProviderCodex)` (🎯T18) for allowance.
- Session/app-server path is out of scope for T14.1 (see
  `docs/codex-app-server-spike.md`).

# Codex Provider Oracle Map

Status: active verification plan for 🎯T4.8.

Codex provider work is mostly new-code oracle mode with two public-contract seams:

- `codex exec --json` for Task mode.
- `codex app-server` JSON-RPC for persistent Session mode.

Live Codex runs are smoke/regression checks only. They do not retire targets by themselves. A target retires when hermetic fixtures, fakes, and fault checks prove the mapping and lifecycle behavior.

| Target | Verification Class | Machine Oracle | Live Role |
| --- | --- | --- | --- |
| 🎯T4.1 provider boundaries | New-code lifecycle seam | Fake Claude and fake Codex task/agent backends drive the same public lifecycle without real binaries. | None |
| 🎯T4.2 Codex binary discovery | Deterministic resolver | Resolver tests inject env, PATH lookup, app-bundle candidates, and missing-binary failure. | Optional manual install sanity |
| 🎯T4.3 Codex Task mode | Public CLI fixture parser | Golden `codex exec --json` fixtures cover thread start, turn start/completion/failure, command/tool progress, agent messages, usage, malformed payloads, and final-message selection. | `CLAUDIA_CODEX_LIVE=1` smoke only |
| 🎯T4.4 app-server contract spike | Public protocol fixture/schema | Generated schema inspection plus golden app-server JSON-RPC fixtures parsed into typed thread, turn, item, usage, interrupt, and failure events. | Explicitly approved live capture records notification order |
| 🎯T4.5 Codex Session mode | Fake app-server lifecycle | Fake app-server exercises Start, Send, WaitForResponse, SubscribeEvents, Interrupt, resume, and capability errors with no real Codex/network/auth. | Gated live app-server smoke only |
| 🎯T4.6 capability gaps | Negative capability oracle | Typed unsupported/experimental errors are asserted for rewind, tmux attach, terminal logs, cost, and permission mapping where public Codex contracts are absent or weaker. | Human review of accepted gaps |
| 🎯T4.7 docs/release gate | Documentation consistency | README, agents-guide.md, STABILITY.md, release notes, and live-test gates name the same support matrix. | Release checklist only |

## Required Golden Fixtures

`testdata/codex/exec/`:

- `success.jsonl`: `thread.started`, `turn.started`, `item.started` command, `item.completed` agent message, `turn.completed` usage.
- `failure.jsonl`: `thread.started`, `turn.started`, `turn.failed`.
- `error.jsonl`: top-level `error`.
- `malformed.jsonl`: invalid JSON and unknown event types, expected to be ignored or surfaced according to parser contract.

`testdata/codex/app-server/`:

- `success.jsonl`: initialize response, thread/start response, turn/start response, item notifications, agent-message deltas or completed messages, turn/completed.
- `failure.jsonl`: turn failure or error response.
- `interrupted.jsonl`: turn/started, turn/interrupt response, turn/completed with interrupted/cancelled status.
- `unsupported-capability.jsonl`: method/field rejection for an experimental or unavailable capability.

Fixtures must be small, redacted, and hand-owned. `TestParseCodexAppServer*`
parses them into typed events so notification order and field mapping are
machine-checked. Do not commit full generated schema bundles unless a future
review finds their license and churn acceptable.

## Fault Checks

Before 🎯T4 is retired, tests must prove these injected faults fail:

- Dropped thread/session id: parser or fake backend emits no session id and lifecycle assertions fail.
- Wrong final-message selection: multiple agent messages arrive and the final result must use the last completed assistant message.
- Malformed usage accounting: cached input and output token fields are swapped or ignored.
- Silent unsupported capability success: Codex rewind/tmux attach/terminal log APIs appear to succeed without a public contract.
- Private-storage shortcut: production Codex provider code reads or writes private Codex transcript/session storage instead of using `codex exec` or app-server. `TestCodexProviderDoesNotReadPrivateStorage` scans production Go files for private Codex state path tokens.

## Human Residue

Machine checks do not decide:

- Whether Codex parity gaps are acceptable for users.
- Whether the public API names feel right before v1.0.
- Whether app-server maturity risk is acceptable for persistent Session mode.

Those require one explicit human accept/reject review after the machine checks pass.

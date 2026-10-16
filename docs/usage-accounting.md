# Durable provider usage observations

Subscription sidecar seats record usage observations in their existing
conversation History as `claudia_usage_v1` custom facts. These entries never
enter model context. The sidecar also mirrors the same typed fact in dated
spool records as `type: usage`, `usage_fact: {...}`. Spool files are an
observation mirror; the conversation journal is the durable query source.

Each observation has a stable `call_id`, a `started` fact and, when
observed, a terminal fact. `seq` identifies each append in the seat/session
usage sequence. `requests` lists the host logical request IDs and their
original acceptance attempt IDs; one call can absorb several inputs, and
one input can require many model calls. A transport `rpc_id` identifies only
a query. Neither the owner request nor the turn is an accounting dedup key.
Physical observations also carry `invocation_id` for the enclosing engine
call and `provider_attempt_id` for one actual HTTP dispatch. The latter is
the `call_id`, never a host delivery attempt. Provider-returned request and
response IDs are optional diagnostics, not dedup keys.

`QueryUsage(ctx, UsageQuery{Seat, SessionID, After, Limit})` reads a bounded
page without loading an agent or accessing credentials. `Next` is the next
cursor, and `More` requests another page. The maximum page size is 128.
`QueryUsageDirect` supplies the broker's library-owned direct seam.
`Agent.RefreshUsage(ctx)` catches up a direct or broker handle with a bounded
deadline. Events carry `Event.UsageFact`; repeated facts add their known
counters to `Agent.Usage()` only once. `Agent.UsageError()` exposes a sticky
observation failure. Replaying old facts is read-only, not a request to
repeat provider work.

Historical queries make one asynchronous cold scan per cached file version,
retaining only validated usage facts. Warm pages recheck file identity and
copy at most 128 facts; they do not reread or parse the conversation.
Append or atomic rewrite invalidates the cached version. Rebuilding must
preserve the cached fact prefix, so normal growth and conversation-only
rewrites remain usable; conflicting changes fail. A file that changes during
scanning fails that query without an automatic retry. Cache eviction loses
prior-prefix mutation detection, not the durable facts. There is no claim of
detecting arbitrary historical edits after eviction or restart.

The historical reader permits one concurrent cold scan, sharing it for the
same session. Other cold queries receive an explicit busy error. It honors
the forwarded deadline and sidecar-connection disconnect with a ten-second
server maximum,
checking between asynchronous 64 KiB reads and bounded record processing.
The original caller expiry crosses the broker unchanged, rather than starting
a fresh timeout there. Early broker-client cancellation does not introduce a
new cancellation protocol: outstanding sidecar work remains bounded by that
forwarded expiry. Filesystem stat/open/read completion is cooperative:
a blocked OS operation itself cannot be preempted by this deadline; the reader
checks it again when that operation returns.
Resource ceilings are 256 MiB of journal bytes, 8 MiB per JSONL record,
50,000 facts, 16 MiB of charged projection memory, and an eight-session LRU
cache capped at 32 MiB charged memory. Charge is four times encoded fact
bytes plus 512 bytes per fact; it is a conservative accounting budget, not a
measurement or guarantee of JavaScript heap size. One cold projection and
record buffers coexist with the cache. Oversize or malformed input fails
explicitly, with no truncated success, file repair, or model load. These are
local diagnostic admission limits, not an established economical profile for
provider context maintenance. Regression tests independently vary fact count
and conversation bytes and measure actual source reads and warm pages.

Counters and estimated dollars are optional. An absent counter is unknown;
a present zero can represent a measured zero in the typed contract. The
unpatched backends observe positive adapter-normalized counters because
the adapters synthesize zero defaults. They declare
`counter_source: adapter_normalized_positive`, `coverage: engine_invocation`.
Patched Anthropic Messages and generic Responses instead declare
`counter_source: provider_reported`, `coverage: provider_attempt`, and retain
explicit raw zero fields. No zero-valued default is promoted to evidence.
Context size is separate
from consumption; reasoning tokens are a subset of output. Optional
`api_equivalent_cost_usd` has `cost_source: provider_adapter_estimate` and
never represents a subscription balance or quota. This physical-attempt
slice leaves dollar estimates absent.

Consumption buckets are disjoint: `input_tokens` is fresh input, with cache
reads and cache creation counted separately. `total_input_tokens` is an
optional inclusive input observation; **use it instead of summing the input
buckets, never in addition to them**. Responses can report this total while
omitting cache details: fresh/cache then remain unknown. Anthropic's total
is derived only when every disjoint input field is known. Reasoning is a
subset of output. Legacy `Agent.Usage()` remains a partial sum of known
disjoint counters; inclusive totals and availability stay in typed facts.

The reproducible Bun patch of `@oh-my-pi/pi-ai@18.2.11` adds one neutral
observer at physical fetch dispatch and raw usage parser boundaries. It
does not alter retries, consume bodies independently, or store another
ledger. Anthropic and generic Responses include HTTP errors, stream
errors, transport retries and otherwise discarded empty-success retries.
Their physical facts replace the engine aggregate, so the two scopes are
never both counted for the same invocation. Historical invocation-only
records retain their original scope. Anthropic native compaction and local
summary calls pass through the existing completion wrapper; compaction
iteration usage replaces the top-level snapshot and is counted once.

The observation path includes explicit auth-retry engine calls and records failed
calls even when the failed assistant message is removed before retry.
Cancellation does not preempt a late result carrying measured usage. A
cancelled stream whose result never settles, like a crash leaving only a
start fact, remains incomplete with unknown consumption.
Negative, nonfinite or unsafe counters are refused and reported as
`usage_error`; accounting append failures poison current journal queries.
The observer neither retries calls nor changes admission. The shared
History's existing terminal-persistence errors remain errors.

This is still a partial lower bound, not complete usage reporting. Codex
and Cursor transport attempts, OpenAI/Codex native compaction, and raw
availability for their counters remain open. The new raw fixtures prove
the Anthropic and generic Responses seams, not every optional upstream
transport, custom client or provider extension. Responses orchestration and
other vendor-specific usage extensions are not fully normalized here;
unproven fields stay absent. Missing raw cache dimensions remain unknown.
Partial usage received before an interrupted stream is retained when the
attempt unwinds; a process crash before terminal append leaves only its
start fact. Historical sessions without facts, lost events before durable
append, and admission under accounting outage remain open coverage.
A successful query or nil
`UsageError()` does not certify these gaps away. Live provider verification
and the full runtime gate remain required before product completion.

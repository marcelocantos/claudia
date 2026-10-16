# Durable invocation usage

Subscription sidecar seats record usage observations in their existing
conversation History as `claudia_usage_v1` custom facts. These entries never
enter model context. The sidecar also mirrors the same typed fact in dated
spool records as `type: usage`, `usage_fact: {...}`. Spool files are an
observation mirror; the conversation journal is the durable query source.

Each engine invocation has a stable `call_id`, a `started` fact and, when
observed, a terminal fact. `seq` identifies each append in the seat/session
usage sequence. `requests` lists the host logical request IDs and their
original acceptance attempt IDs; one call can absorb several inputs, and
one input can require many model calls. A transport `rpc_id` identifies only
a query. Neither the owner request nor the turn is an accounting dedup key.

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
the caller's forwarded deadline/disconnect with a ten-second server maximum,
checking between asynchronous 64 KiB reads and bounded record processing.
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
current producer only observes positive adapter-normalized counters because
the pinned adapters synthesize zero defaults. It declares
`counter_source: adapter_normalized_positive`, `coverage: engine_invocation`.
No zero-valued default is promoted to evidence. Context size is separate
from consumption; reasoning tokens are a subset of output. Optional
`api_equivalent_cost_usd` has `cost_source: provider_adapter_estimate` and
never represents a subscription balance or quota.

The first slice covers the engine stream invocation (including explicit
auth-retry calls) and local compaction completion calls. It records failed
calls even when the failed assistant message is removed before retry.
Cancellation does not preempt a late result carrying measured usage. A
cancelled stream whose result never settles, like a crash leaving only a
start fact, remains incomplete with unknown consumption.
Negative, nonfinite or unsafe counters are refused and reported as
`usage_error`; accounting append failures poison current journal queries.
The observer neither retries calls nor changes admission. The shared
History's existing terminal-persistence errors remain errors.

This is a partial lower bound, not complete usage reporting. Hidden
HTTP/WebSocket retries and provider-native remote compaction are not
individually observed. Known-zero/raw-provider evidence, historical sessions
without these facts, lost events before durable append, and admission under
accounting outage remain open coverage. A successful query or nil
`UsageError()` does not certify these gaps away. Live provider verification
and the full runtime gate remain required before product completion.

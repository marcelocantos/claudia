# Bounded live accounting check

`make live-usage` checks one short owner input on each subscription backend:
Anthropic, Codex, Grok and Cursor. It compares physical facts from actual provider
responses with the SDK event deltas, `Agent.Usage()`, canonical direct/broker
queries, repeated replay, and a detached/reclaimed broker handle. It sends no
second prompt. All four providers must be available; an enabled run never skips
an unhealthy provider. This is a Claudia wire check, not a Jevons product journey.

## Prepare outside the test

An operator must activate an isolated broker **built from the same clean commit
as this test and its sidecar**, using a previously authorized trusted executable.
The test does not install a binary, change Keychain trust, export credentials,
start a broker, or initiate login/refresh. Do not overwrite the installed trusted
binary merely to make this check run. If no trusted current build exists, live
SDK/broker verification remains outstanding until that setup is authorized.

The installed v0.53.0 broker can launch a new sidecar via `CLAUDIA_OMP_SERVER`,
but discards the new typed usage events and lacks the canonical usage RPC. It is
not valid evidence for this check. `StartDirect` and adoption also require the
caller's own in-memory credentials; they cannot borrow the broker's credential
store. The test uses the real SDK broker grant path directly so a vanished broker
cannot cause `Start`'s normal fallback to direct execution.

Use a new canonical temporary directory named `claudia-usage-live.<unique>`.
The activated broker and the test must use the following environment, with
absolute paths (resolve macOS `/tmp` to `/private/tmp`):

```text
CLAUDIA_USAGE_LIVE=1
CLAUDIA_USAGE_ROOT=<root>
CLAUDIA_USAGE_ACTIVATION=<root>/activation.json
CLAUDIA_BROKER_SOCKET=<root>/broker.sock
CLAUDIA_OMP_SOCKET=<root>/omp.sock
XDG_STATE_HOME=<root>/state
JEVONS_SPOOL_DIR=<root>/spool
CLAUDIA_OMP_SERVER=<clean-source>/sidecar/server.ts
CLAUDIA_NO_BROKER=0
CLAUDIA_OMP_NO_REFRESH=1
GOWORK=off
```

Activate with empty isolated broker state, `--no-resume` and `--restart-nudge -`.
Keep the real user home for the already trusted credential owner; isolate state
and spool explicitly instead. OAuth refresh must be disabled in the **broker**,
not only the test environment. A missing/expired token is OUTAGE, requiring
separate authorized recovery. Install the source's frozen sidecar dependencies
before activation. Do not modify that source or its dependencies while serving.
Keep other workloads off these sockets. Shut down only this isolate afterward.

Write an operator activation declaration with these JSON fields:

```json
{
  "Root": "/private/tmp/claudia-usage-live.example",
  "SourceDir": "/private/tmp/clean-claudia-source",
  "Commit": "<full clean commit SHA>",
  "Tree": "<full tree SHA>",
  "BrokerBinary": "<absolute canonical trusted current-build executable>",
  "BrokerSHA256": "<SHA256 of that executable>",
  "BrokerPID": 123,
  "SidecarPID": 124,
  "BrokerStart": "<ps -o lstart= for broker, trimmed>",
  "SidecarStart": "<ps -o lstart= for sidecar, trimmed>",
  "NoRefresh": true,
  "Models": {
    "anthropic": "<explicit small supported model>",
    "openai-codex": "<explicit supported model>",
    "xai-oauth": "<explicit supported model>",
    "cursor": "<explicit supported model>"
  }
}
```

These are declarations of what was activated, not runtime cryptographic
attestation. The test verifies clean source commit/tree, broker binary hash and
Go VCS build metadata, process start times/executable, socket ownership and
listening status. It cannot prove which JavaScript bytes a process loaded before
the operator recorded activation. The operator must bind that fact honestly.

With that environment set, run `bin/gate -- make live-usage` (or the host's
absolute `bin/gate` path). This uses the existing exclusive live-test lock.
An unset gate skips under ordinary hermetic tests and is **not live evidence**.

## Bounds and limits of the evidence

Each provider has 90 seconds; the whole test has an eight-minute context and a
ten-minute suite backstop. There is one input per provider, no goal, no MCP
servers, and a read-only sandbox. The prompt requests no tools; any observed
tool use fails and interrupts the slice. The session API does not expose a hard
zero-tool or per-physical-request admission cap. Built-in provider retries remain
unchanged, and cancellation is best effort. This is not a spend-limit guarantee;
physical admission control remains separate work.

Known counters are compared per field without adding inclusive input to cache
buckets. Absence remains unknown in the typed facts. `Agent.Usage()` and event
`Usage` are only the partial sum of known disjoint counters. Cursor's protocol
token deltas are never billed consumption; its billing dimensions remain unknown.
Facts are logged without prompts, provider response text, or credentials.
API-equivalent cost is never subscription quota.

The small fresh conversations do not deliberately exercise compaction, hidden
retries, auth repair, cancellation, or provider failures. Frozen actual-adapter
fixtures cover those branches separately; this check only proves the transport
and dimensions observed in its own real calls. Provider server-internal work and
subscription balances remain outside this measurement.

# Subscription live harness (T171)

Implementation is in progress until integrated and the authorized live evidence
is owner-visible. Hermetic green is not an activation or a live pass.

The old `jevons-broker refresh-plans` and `smoke` commands no longer exist in the
current CLI. These tests use an explicitly selected installed `claudia` binary,
its `broker auth-status --json`, `broker auth-recover --no-login PROVIDER`, and
`broker grant/send/interrupt/release` commands. They never discover a sibling
checkout binary or start a sidecar implicitly.

## Operator inputs before an authorized run

- `CLAUDIA_OMP_LIVE=1` opts into the row. Do not set it while running hermetic
  contracts. T170 owns the two path refusal tests and the separate native ACL
  probe; a copied CLI issuing broker RPCs is never ACL evidence.
- `JEVONS_BROKER_BIN`: absolute installed `claudia` executable. Symlinks resolve
  to their canonical path. This legacy variable name remains for T170 helpers.
- `CLAUDIA_BROKER_SOCKET` and `CLAUDIA_OMP_SOCKET`: distinct absolute sockets of
  an already running, reviewed broker and sidecar.
- `CLAUDIA_OMP_ACTIVATION_RECEIPT`: absolute JSON file with the schema below.
- `CLAUDIA_OMP_LIVE_PLAN`: exactly one of `anthropic`, `openai-codex`, `cursor`,
  `xai-oauth`.
- `CLAUDIA_OMP_LIVE_PROVIDER`: `cursor` or `grok` for smoke; these CLI provider
  IDs select subscription sessions. Smoke spends turns in a uniquely named
  temporary work seat and releases that seat at cleanup.

Receipt example (placeholders must be replaced with observations):

```json
{
  "broker_binary": "/absolute/installed/claudia",
  "broker_sha256": "sha256 of that installed executable",
  "broker_socket": "/absolute/broker.sock",
  "broker_pid": 123,
  "broker_start": "exact trimmed ps -p 123 -o lstart= output",
  "sidecar_socket": "/absolute/sidecar.sock",
  "sidecar_pid": 124,
  "sidecar_start": "exact trimmed ps -p 124 -o lstart= output",
  "loaded_artifact_evidence": "path/reference to reviewed activation log and immutable sidecar manifest"
}
```

The preflight checks the binary digest, process start times, broker executable
path, each PID's ownership of its socket via `lsof`, socket connectivity and the
sidecar PID file. It refuses missing/default inputs. It does not attest the
loaded TypeScript itself. The operator's activation evidence must bind the
serving sidecar PID/start time to the reviewed immutable artifact directory,
entry point and dependency manifest observed at startup, with no subsequent
mutation. Hashing a mutable `server.ts` after startup is insufficient. If that
chain cannot be established, activation identity remains unproven: do not use
this receipt to claim it. No activation or restart is performed by preflight.

## Recovery and smoke evidence limits

`TestT865LiveBrokerRefreshPlans` performs supported no-login recovery, checks
health before and after, then explicitly skips the renewal claim. Healthy
no-op, auth health and allowance/usage refresh never prove renewal. The current
RPC exposes no refreshed/no-op outcome; a stronger live renewal oracle remains
outstanding. Do not expire, reject or overwrite shared tokens to force it.
`TestT171RecoveryRenewsExpiredPlan` pins expired-token renewal, persistence and
subsequent healthy no-op hermetically. T165 pins refused renewal without login.

Smoke proves a grant and sentinel response, a steer-mode acknowledgement and
an interrupt acknowledgement. OMP currently returns no steer mechanism, so
model uptake of steer and observed abort termination remain stronger live
oracles to obtain. It does not claim `jevons_*` host-tool arming: those tools
belong to the host, and the standalone CLI cannot attest them. There is no
fabricated `smoke` success string standing in for those oracles.

## Coordinated jevonsd bounce only

`CLAUDIA_OMP_BOUNCE_AUTHORIZED=restart-jevonsd` is an explicit authorization
receipt from the coordinator, not a value to set merely to unskip a test.
Also supply `CLAUDIA_OMP_BOUNCE_ARGV` and `CLAUDIA_OMP_READY_ARGV` as JSON argv
arrays with absolute executable paths. The coordinator must ensure the first
restarts only the named jevonsd service and the second checks that service's
readiness (not just supervisor process existence). No shell string is evaluated.
The restart plus readiness loop has a 90-second bound; each readiness probe
has a 3-second bound. After readiness, preflight requires the same broker and
sidecar PIDs/start times and both sockets. No `Ensure`, default socket, implicit
supervisor selection or uncoordinated shared restart is allowed.

The process/socket checks use macOS `ps` and `lsof`; the future live run needs
permission to observe those processes. Hermetic contract runs do not invoke
them, contact the real sockets, or mutate credentials.

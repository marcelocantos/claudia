# Isolated broker usage fixture (test-only)

Launch a **test** broker with an explicitly created private state directory (owner-owned, mode 0700, absolute, no symlink components or non-private entries):

```sh
state=$(mktemp -d /private/tmp/claudia-fixture.XXXXXXXX)
claudia broker serve --state-dir "$state" --test-usage-fixture
```

The flag refuses the broker's default state directory, an external consumer state/seed, and a broker socket override outside the private directory. It disables provider credential reads, sidecar startup, seat resumption, model-intel refresh and vendor usage polling. It does not alter an ordinary broker. It creates `usage-fixture.sock` (0600) and a fresh per-run `usage-fixture.cap` (0600) in that directory. This socket is **not** the public broker protocol or the SQL replication control socket. Never use this mode against an owner broker. Remove the private state directory after the test (after stopping the process).

POST `http://fixture/usage` over that Unix socket, with `X-Fixture-Capability` set to the contents of the capability file. The body is `{ "backends": [PlanUsage, ...] }`; each `PlanUsage` has the ordinary Claudia JSON schema (`provider`, `status`, `windows` with `name`, `remaining_percent`, `used_percent`, `resets_at`, etc.). `status:"available"` requires a published window; `remaining_percent:0` classifies as exhausted. A 429 must instead be `status:"unavailable"` with no windows. The broker stamps observation time, classifies window bands and commits the entire submitted provider set in one broker-owned control-file transaction, through the same row/history writer as real polling. Providers omitted from a POST retain their previous reading, allowing J34 to update Claude and destination only. HTTP 200 means committed, **not** replicated: wait for the next observed `claudia_plan_usage.success_at` generation on a separately linked consumer replica before asserting state. Consumer-side SQL writes are not part of this API. The public broker `usage` request remains read-only.

The Claudia hermetic CLI/replica tests assert the actual typed `PlanUsage` and `ClassifyPlan` bands, not JSON substrings: J20 goes Grok weekly 30% remaining/70% used with a half-elapsed week (**ahead**) → Grok 90/10 with a 10%-elapsed week (**ok**) → one four-provider Grok/Claude/Codex/Cursor 0/100 transaction (**exhausted**) → all four 90/10 (**ok**). A malformed batch and wrong capability are refused. The explicit broker default state directory is refused before any log or database write.

The J34-*shaped broker* oracle starts with Claude and Grok healthy at a **simulated** pre-seat boundary, updates Claude to exhausted and Grok healthy after that boundary, disconnects/reconnects a replica while the broker stays up and checks the persisted hot+healthy readings. It also tests an optional broker restart separately. **It does not start a real Jevons seat, migrate it, bounce Jevons daemon, or assert SessionID continuity.** The full owner journey remains 🎯jevons/T1234: start Claude seat, push hot Claude and healthy destination, migrate in place, then restart only the isolated Jevons consumer daemon and assert provider/SessionID and reconnect state. Neither the hermetic Claudia test nor broker persistence substitutes for that system oracle. No production broker, consumer, or vendor state is involved in the Claudia tests.


## Topology B: ordinary isolated broker with per-provider override (🎯T234)

`--test-usage-fixture` cannot host live seats: it skips OMP credentials and the sidecar. Topology B is a **separate** flag on an ordinary isolated credential-capable broker:

```sh
state=$(mktemp -d /private/tmp/claudia-override.XXXXXXXX)
claudia broker serve --state-dir "$state" --usage-fixture-override=grok
```

OMP plan reads, sidecar, Launch/Send, resume and the one broker sqlpipe replica stay on the ordinary path. Only the CLI-fixed allowlist (here `grok`) is intercepted. Every other provider keeps the real broker fetch.

The flag is refused when:

- combined with `--test-usage-fixture`
- the state directory is missing, relative, the default broker state, a symlink, or not mode 0700
- the process is in a supervisor/prod context (`SUPERVISOR_ENABLED` / `SUPERVISOR_SERVER_URL` / `SUPERVISOR_PROCESS_NAME`)
- `CLAUDIA_BROKER_SOCKET` or `--socket` names the production socket or a path outside the private state directory
- `--mcp-seed-dir` or `--consumer-state-dir` escapes the private state directory
- `CLAUDIA_CONSUMER_STATE_DIR` is set (when `--consumer-state-dir` is empty) to a path that escapes the private state directory — the same fallback `daemon` uses for fleet intent
- the allowlist names an unknown provider

Authorization is a per-run capability file at `<state>/usage-fixture-override/capability` (0600). POST `http://override/usage` over `<state>/usage-fixture-override/control.sock` with `X-Fixture-Capability`. Unenrolled providers and a wrong capability are refused; a successful POST commits through the same broker-owned control-file writer as real polling.

This is not the Jevons J34 live migration oracle. J34 remains 🎯jevons/T1234.

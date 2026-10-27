# Isolated broker usage fixture (test-only)

Launch a **test** broker with an explicitly created private state directory (owner-owned, mode 0700, absolute, no symlink components or non-private entries):

```sh
state=$(mktemp -d /private/tmp/claudia-fixture.XXXXXXXX)
claudia broker serve --state-dir "$state" --test-usage-fixture
```

The flag refuses the broker's default state directory, an external consumer state/seed, and a broker socket override outside the private directory. It disables provider credential reads, sidecar startup, seat resumption, model-intel refresh and vendor usage polling. It does not alter an ordinary broker. It creates `usage-fixture.sock` (0600) and a fresh per-run `usage-fixture.cap` (0600) in that directory. This socket is **not** the public broker protocol or the SQL replication control socket. Never use this mode against an owner broker. Remove the private state directory after the test (after stopping the process).

POST `http://fixture/usage` over that Unix socket, with `X-Fixture-Capability` set to the contents of the capability file. The body is `{ "backends": [PlanUsage, ...] }`; each `PlanUsage` has the ordinary Claudia JSON schema (`provider`, `status`, `windows` with `name`, `remaining_percent`, `used_percent`, `resets_at`, etc.). `status:"available"` requires a published window; `remaining_percent:0` classifies as exhausted. A 429 must instead be `status:"unavailable"` with no windows. The broker stamps observation time, classifies window bands and commits the entire submitted provider set in one broker-owned control-file transaction, through the same row/history writer as real polling. Providers omitted from a POST retain their previous reading, allowing J34 to update Claude and destination only. HTTP 200 means committed, **not** replicated: wait for the next observed `claudia_plan_usage.success_at` generation on a separately linked consumer replica before asserting state. Consumer-side SQL writes are not part of this API. The public broker `usage` request remains read-only.

J20: first publish Grok weekly 30% remaining (ahead/under), then Grok 90%, then publish all four tracked providers at 0% in one POST, then restore them to healthy. J34: start the Claude seat while healthy, publish Claude 0% and a healthy destination, and restart the *isolated Jevons consumer daemon* after migration; the broker stays up. A separate broker-restart persistence test is useful but does not stand in for that J34 journey. No production broker, consumer, or vendor state is involved.

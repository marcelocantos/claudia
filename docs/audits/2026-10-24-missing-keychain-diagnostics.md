# Missing Keychain diagnostics mistaken for credentials

J43's `invalid character 's'` error does not establish corrupt credential
contents. The production runner returns stderr on command failure. `readItem`
accepted a missing-item error, then tried to decode those diagnostic bytes as
plan JSON. A normal `security: ... could not be found ...` message reproduces
the reported error through the real `Open` caller.

The isolated correction is `3a539c3` on `codex/keychain-missing-diagnostic`,
worktree `/tmp/claudia-keychain-missing-diagnostic-20261024`, based on `064b0d37`.
It returns the existing empty-store outcome immediately for a missing item.
It preserves malformed successful output and non-missing read errors.

`TestT132MissingItemDiagnosticsAreNotCredentialContents` uses a synthetic
runner, invokes `Open`, checks missing diagnostics, malformed successful
output, denied reads, and an existing empty item. It also checks one read and
preservation of the denied error's identity. The pre-fix run reproduced the
exact error: gate `f575a90f`, RED. The full `./omp` package verification is
queued on the shared heavy lease; no pass is claimed here.

No actual credential was read or changed, and no provider was called. This
is parser-branch coverage, not proof of live authentication or T132 recovery.
T132.1 records this narrow gap; J43 and the parent T132 remain open. Compose
and verify the candidate before activation. Do not modify the frozen 9f33 or
064b evidence checkouts in place to include it.

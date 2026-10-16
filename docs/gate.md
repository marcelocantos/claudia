# Owner gate

Claudia owner-ships to `master` by gated push. There is no owner
release-prep PR.

```bash
make hooks          # git config core.hooksPath=scripts/hooks
make gate           # hermetic suite (pre-push oracle)
git push origin master
```

`scripts/hooks/pre-push` builds and vets, then checks the local attestation
for the tree of each pushed tip. It does not rerun the slow gate while the
Git transport is open. Agents must not pass `--no-verify`. Inbound
contributor PRs stay; CI on `pull_request` is courtesy for those.

`make gate` requires a clean committed checkout, including untracked files.
One process holds a per-clone lock, clones the committed revision into a
temporary directory, and runs `gate-checks` there with `GOWORK=off` and
read-only Go module resolution. It does not copy ignored files or existing
sidecar dependencies. An inherited Make include or Git repository selector
cannot redirect the run. Concurrent gate attempts refuse before touching the
active run's evidence. Old begin/end invocations are rejected.

The checks are the sidecar prerequisite probe, a frozen Bun install and its
behavioral tests, then `go vet`, `go test -race -count=1 ./...`,
`verify-stability`, and `verify-mutation-evidence`. Bun, Unix sockets and
(on macOS) sandbox execution are required; an unavailable prerequisite is
an explicit OUTAGE. Linux does not prove the macOS seatbelt behavior.
Live backends are
`make live` (opt-in env gates) and are a release-time owner check when
the provider wire changed — not the hook.

Only a successful run with clean, unchanged source and snapshot trees writes
an atomic `full-runtime-v1` attestation in the calling clone's git directory.
Starting a new run revokes its older record; a failed run cannot fall back to
that earlier pass. The hook rejects legacy attestations that omitted the
sidecar suite. Snapshot cleanup happens before publication. A shared-checkout
edit that is reverted during the run cannot contaminate the isolated source.

For fast WIP diagnosis, `make gate-checks` and `make test-sidecar` run directly
without certifying the tree. Commit before using `make gate`. Its snapshot
retains local history/tags for the stability and mutation checks, but its
working files and dependency install are private. This is isolation from
concurrent checkout edits, not a sandbox against malicious test code.

The live gate's own coverage rides the hermetic suite:
`internal/livegate` reads every live test out of the source and fails
when one is unreachable from `make live` or unnamed in AGENTS.md's
table, so the two documents that tell an agent what to run cannot
quietly fall behind the tests (T100). Deliberate exceptions live in
`live-gate-exclusions.json` with a stated reason.

`make gate-full` adds the TLA+ broker spec (`verify-specs`).

A stray `.go` file cannot take this gate down with it: in-repo scratch
lives under `_scratchpad/`, which the go command ignores, and
`scratch_invisible_test.go` holds that property. See AGENTS.md,
"Scratch files stay invisible to the toolchain".

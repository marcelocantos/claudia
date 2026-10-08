# Owner gate

Claudia owner-ships to `master` by gated push. There is no owner
release-prep PR.

```bash
make hooks          # git config core.hooksPath=scripts/hooks
make gate           # hermetic suite (pre-push oracle)
git push origin master
```

`scripts/hooks/pre-push` runs `make gate` and refuses a non-zero exit.
Agents must not pass `--no-verify`. Inbound contributor PRs stay.
Colossus (macOS + keychain) is the runtime for all development and
testing. GitHub stores the code as backup. There is no GitHub Actions
workflow: push and pull request do not run a CI job.

`make gate` is `go vet`, `go test -race -count=1 ./...`,
`verify-stability`, and `verify-mutation-evidence`. Live backends are
`make live` (opt-in env gates) and are a release-time owner check when
the provider wire changed — not the hook.

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

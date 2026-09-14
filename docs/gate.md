# Owner gate

Claudia owner-ships to `master` by gated push. There is no owner
release-prep PR.

```bash
make hooks          # git config core.hooksPath=scripts/hooks
make gate           # hermetic suite (pre-push oracle)
git push origin master
```

`scripts/hooks/pre-push` runs `make gate` and refuses a non-zero exit.
Agents must not pass `--no-verify`. Inbound contributor PRs stay; CI
on `pull_request` is courtesy for those.

`make gate` is `go vet`, `go test -race -count=1 ./...`,
`verify-stability`, and `verify-mutation-evidence`. Live backends are
`make live` (opt-in env gates) and are a release-time owner check when
the provider wire changed — not the hook.

`make gate-full` adds the TLA+ broker spec (`verify-specs`).

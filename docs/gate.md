# The owner gate

Claudia has one human. Owner work lands on `master` by a gated push, not by
a pull request: the gate is a local oracle that runs before every push and
refuses the push when it is red. GitHub Actions runs the same recipe after
the fact as a second-platform regression net. Nothing waits on a PR.

Target: claudia 🎯T48, the guinea pig for think 🎯T8 (owner-solo repos ship
to the default branch via a local gate).

## Flow

```
edit → commit → git push origin master
                   │
                   ├─ scripts/hooks/pre-push   (core.hooksPath=scripts/hooks)
                   │     └─ make gate          exit ≠ 0 → push refused
                   │
                   └─ origin/master updated
                         └─ .github/workflows/test.yml runs `make gate`
                            on ubuntu + macos (push to master, tags v*)
```

## Wiring

Once per clone:

```bash
make hooks        # git config core.hooksPath scripts/hooks
```

A relative `core.hooksPath` resolves against the worktree the push runs
from, so one setting covers every `git worktree`. `make gate` prints a
warning when the setting is absent (not under `GITHUB_ACTIONS`); the
warning does not fail the gate, because the hook is what runs the gate.

Redirecting `core.hooksPath` makes git read *only* `scripts/hooks`; anything
in `.git/hooks` is disabled. In an LFS repo that silently kills LFS's
`pre-push` (pointers reach the remote with nothing behind them). Claudia
tracks no LFS objects, and `scripts/hooks/pre-push` chains
`git lfs pre-push "$@"` on the saved stdin ref list anyway, so a future
`git lfs track` cannot break uploads without anyone noticing.

### Git's exported environment (`nogit`)

Git exports `GIT_DIR`, `GIT_INDEX_FILE`, `GIT_WORK_TREE`, `GIT_OBJECT_DIRECTORY`
and friends into every hook, and **every process the hook spawns inherits
them** — including the whole of `make gate`. A test that shells out to git
against a scratch repo then operates on the *pushing* repository instead,
because the environment overrides the scratch directory's own `.git`.

This is not hypothetical. On 2026-09-06 a hook drill in `bullseye` flipped
that repo's primary checkout to `core.bare=true`, rewrote its `.git/config`
and landed a spurious commit — from a test that ran `git init` in a temp
directory. The drill only exposed it; a genuine `git push` would have done
the same damage.

`scripts/hooks/pre-push` therefore strips the variables before running
anything:

```bash
nogit() {
	env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_COMMON_DIR \
		-u GIT_PREFIX -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES \
		-u GIT_QUARANTINE_PATH -u GIT_PUSH_CERT_NONCE -u GIT_REFLOG_ACTION "$@"
}

if [[ "$updates" -gt 0 ]]; then
	cd "$(nogit git rev-parse --show-toplevel)"
	if ! nogit make gate; then
		...
	fi
fi
```

Both call sites need it. `--show-toplevel` is not exempt: with `GIT_DIR`
pointing at the common dir of a multi-worktree repo, it names the wrong
worktree and the gate runs against the wrong tree.

The trailing `git lfs pre-push "$@"` chain is deliberately **not** wrapped —
git-lfs is a git subcommand and needs the environment git handed it.

## What `make gate` runs

| Step | Oracle | Owned by |
|------|--------|----------|
| `gofmt -l .` | every Go file formatted | gate |
| `go vet ./...` | vet clean | gate |
| `scripts/gate-test.py` | `go test -json -race -count=1 -shuffle=on ./...` | gate |
| `make verify-stability` | STABILITY.md matches the snapshot tag's real public surface (🎯T29) | gate |
| `make verify-mutation-evidence` | quoted mutation evidence still bites, and the check can be shown to fail (🎯T32) | gate |

Not on the hook:

- `make gate-full` adds `verify-specs` (TLA+ broker lifecycle, needs Java +
  tla2tools). CI runs it in `specs.yml` on every push to master; run it
  locally before a release.
- `make live` runs the real-backend tests under the `CLAUDIA_*_LIVE` gates.
  Hermetic fakes cannot decide spawn, submit, auth, or turn-loop behaviour;
  live runs are a hard gate for provider-wire changes and a release-time
  owner gate (AGENTS.md "Live tests"). CI never sets the gates.

## The test runner and its residue ledger

`go test ./...` says whether anything failed and nothing about what did not
run. Every `t.Skip` in this suite turns a test into a no-op that prints
`ok`; a runner without tmux would pass the whole Session suite by not
running it. `scripts/gate-test.py` runs `go test -json` and:

- prints failures with their full output and the `-shuffle` seed that
  produced the order, so it can be replayed with
  `go test -race -count=1 -shuffle=<seed> <pkg>`;
- suppresses a passing test's logging (the suite's INFO lines no longer
  bury the verdict);
- lists every skipped test with its reason, classified against the residue
  this repo declares in the script: **live gate** (`make live` owns it),
  **subprocess helper** (child side of a crash-survival test), and
  **host-bound** (a path that can only be exercised on one machine, such as
  the Claude-binary miss path on a host that has `/opt/homebrew/bin/claude`);
- fails the gate on any skip that matches none of them. `tmux not
  installed`, `python3 required`, `sqlite3 not on PATH` are broken runners,
  not residue.

The final line is the headline: `✓ tests: N passed, 0 failed, M skipped
(... live gate (make live), ... subprocess helper, ...)`. A new class of
residue is declared by adding a pattern to `RESIDUE` in the script, in the
same commit that introduces the skip.

`-shuffle=on` runs each package's tests in a random order every time, so an
order dependency between tests surfaces on the owner's machine rather than
on a runner months later. `-count=1` defeats the test cache; `-race` is
mandatory (🎯T17 made the hermetic Cancel/Stop tests deterministic under it).

## Drill

The gate is only evidence if it has been shown to refuse. The drill, run
without touching the remote:

```bash
git checkout -b drill
cat > drill_test.go <<'EOF'
package claudia

import "testing"

func TestGateDrillPlantedFailure(t *testing.T) { t.Fatal("planted") }
EOF
git add drill_test.go && git commit -qm 'drill: planted failure'

# Invoke the hook exactly as git would: argv = remote name + URL,
# stdin = "<local ref> <local sha> <remote ref> <remote sha>".
# `git hook run` gives the hook /dev/null on stdin unless --to-stdin
# names a file; piping into it is silently an empty push list.
sha=$(git rev-parse HEAD)
printf 'refs/heads/drill %s refs/heads/master %s\n' "$sha" "$sha" > refs.txt
git -c core.hooksPath=scripts/hooks hook run --to-stdin=refs.txt pre-push \
  -- origin https://github.com/marcelocantos/claudia.git
echo "exit=$?"          # must be non-zero: push refused

git rm -q drill_test.go && git commit -qm 'drill: remove planted failure'
sha=$(git rev-parse HEAD)
printf 'refs/heads/drill %s refs/heads/master %s\n' "$sha" "$sha" > refs.txt
git -c core.hooksPath=scripts/hooks hook run --to-stdin=refs.txt pre-push \
  -- origin https://github.com/marcelocantos/claudia.git
echo "exit=$?"          # must be 0: push allowed
rm refs.txt
```

`git hook run` honours `-c core.hooksPath`, so the drill works in a clone
that has not run `make hooks`. The drill runs the gate with git's
hook environment set, so it is also the check that the `nogit` guard above is
still in place: a drill that mutates any repository other than the drill
branch's own worktree means the guard has regressed. The last drill transcript is recorded in the
🎯T48 lifecycle note in `bullseye.yaml`.

## Bypass

`git push --no-verify` skips the hook. It is the owner's emergency bypass
and nothing else: agents do not use it, and the harness permission rules
deny it. A push that needed the bypass is a push whose gate is wrong, and
the gate is fixed next.

## Release

`/release` on this repo: commit the prep on `master`, `make gate` (the
hook repeats it), `make live` if the diff touched a provider wire,
`git push origin master`, then `gh release create` on the tag. No prep PR.
`test.yml` and `specs.yml` run on the tag push as well as on master.

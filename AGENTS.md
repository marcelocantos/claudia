# claudia

Go library for embedding Claude, Grok, Codex, Bedrock, Ollama, and Cursor agents,
and for asking TypeSafe's Jev typed questions (Judge mode).
Consumer API: [`agents-guide.md`](agents-guide.md). This file is for
agents working *in* this repo.

```bash
make gate                      # hermetic owner gate; attests the tree for pre-push
make live                      # real backends; each live env is opt-in
make supervisor-install        # host daemon under supervisord (evicts brew/launchd)
```

## Delivery

Owner ships to `master` by gated push (`make gate`, then
`git push origin master`). Ship only when asked. Do not open an owner
release-prep PR. Inbound PRs from others stay. After clone:
`make hooks` (or `git config core.hooksPath scripts/hooks`).
Flow: [docs/gate.md](docs/gate.md).

## Shared checkout: stage through the index only, never write the working tree

This repo's checkout is shared: several seats routinely hold
uncommitted work in it at once. When you need to commit only *some* of
a path's content — a subset of a file's lines, or your own entries in
a file others are also appending to — stage that content through the
git **index**, and never run a command that overwrites the **working
tree** on a path you do not own.

Safe pattern (content-scoped staging, working tree untouched):

```bash
# build the exact bytes you want committed for the path, e.g. into /tmp/content
git hash-object -w /tmp/content                 # -> blob SHA
git update-index --cacheinfo 100644 <blob-sha> path/to/file
git commit -m "..."                              # working tree never touched
```

If a first attempt at this goes wrong, back it out by re-staging
(`git update-index --cacheinfo` again, or `git reset` the index entry),
**not** by restoring the file from disk or from the index.

**Never run these on a path you do not own**, in a checkout other
seats are working in:

- `git checkout-index` (with or without `-f`)
- `git restore`
- `git checkout -- <path>`
- `git stash` (touches every unstaged path, not just yours)

Each of these overwrites the **working tree**, not just the index.
`git checkout-index -f -- <path>` in particular writes the index's
version of a file over whatever is sitting in the working tree —
it is not an "unstage", and if the working-tree content was never
committed, there is no reflog and no built-in way to get it back.

This bit for real (🎯T102, 2026-09-21): a seat committing its own three
entries in a shared JSON file, while another seat's two uncommitted
entries sat in the same file, used the index trick correctly to select
only its own content — then, backing out a first attempt that had
reformatted the file, ran `git checkout-index -f -- <path>`. That
overwrote the working tree and destroyed the other seat's uncommitted
entries. They were recovered only because a scratch copy happened to
exist elsewhere on disk (`/private/tmp/...`) and matched an earlier
`git diff` capture — luck, not a property of the process. See 🎯T102's
ledger context for the full incident writeup, including a subsequent
correction of who the destroyed work actually belonged to.

## Live tests (backend changes)

Hermetic `go test` cannot decide spawn, submit, auth, or turn-loop
behaviour. A fake that always answers is the world in which a host
loop looks perfect.

**This is a hard gate, not a suggestion.** When you change how a
provider is started, spoken to, or observed, you run the live tests
before calling the work done.

Applies to:

- `Start` / `Send` / `WaitForResponse` / `Interrupt` / `Stop`
- Event mapping, turn identity, terminal detection
- Goal continuation, sandbox, auth preflight, binary discovery
- MCP attach (`LoadMCP`, `Config.MCPServers`)
- sidecar / app-server / ACP / exec framing

**Run every backend whose wire you touched.** A Session-wide change
(Goal, `Send`, events) is every Session backend you can authenticate,
not just the one you had in mind. T39: hermetic journeys plus live
Grok and Codex were green while Claude hung on a multi-line
continuation that only the live TUI paste path could show.

**Backend wires.** Each row is one gate variable and the tests it
un-skips. If you touched the surface, the row is what you must run.

| Gate | Surfaces | Must include |
|------|----------|--------------|
| `CLAUDIA_LIVE=1` | Claude Task and Session | `TestTaskRunSmoke`, `TestClaudeTaskDisallowToolsLiveSmoke`, `TestModelObservableLive`, `TestModelNotFoundLiveFailLoud`, `TestAgentReadinessSmoke`, `TestAgentReadinessUntrustedGitWorkdirLive`, `TestAgentReadinessFailureOnDeadProcess`, `TestAgentSendAndWaitForResponse`, `TestAgentMultiTurn`, `TestRunHelper`, `TestRewindSessionLive`, `daemon.TestRewindLive`, `daemon.TestGoalCompleteCheckLive`, `TestGoalJourneyLiveBackends/claude`, `TestMCPLiveLoadAndSessionSeesMnemo/claude`, `TestMCPHostLiveSeatsSeeMnemo/claude` |
| `CLAUDIA_GROK_LIVE=1` | Grok Task + Session, xAI Realtime | `TestGrokTaskRunSmoke`, `TestGrokSessionLiveSmoke`, `TestGrokSessionLiveSmokeSteer`, `TestExclusiveGrokSessionResumeLive`, `TestGrokPlanUsageLive`, `grok.TestLiveConnect` (also needs `XAI_API_KEY`), `TestGoalJourneyLiveBackends/grok`, `TestMCPLiveLoadAndSessionSeesMnemo/grok`, `TestMCPHostLiveSeatsSeeMnemo/grok`, `TestMCPExclusiveSessionRoundTrip` |
| `CLAUDIA_CODEX_LIVE=1` | Codex Task + Session | `TestCodexTaskRunSmoke`, `TestCodexTaskGitWriteLiveSmoke`, `TestCodexBrokerTaskWriteDefaultLiveSmoke`, `TestCodexBrokerGrantWriteDefaultLiveSmoke`, `TestCodexSessionLiveSmoke`, `TestCodexGitWriteLiveSmoke`, `codex.TestLiveCodexTaskRun`, `codex.TestLiveCodexStandaloneGitWriteCommit`, `TestGoalJourneyLiveBackends/codex`, `TestMCPLiveLoadAndSessionSeesMnemo/codex`, `TestMCPHostLiveSeatsSeeMnemo/codex`, `TestMCPCodexSeatCallsToolLive` |
| `CLAUDIA_BEDROCK_LIVE=1` | Bedrock Task | `TestBedrockTaskLiveSmoke` |
| `CLAUDIA_OLLAMA_LIVE=1` | Ollama Task | `TestOllamaTaskLiveSmoke` |
| `CLAUDIA_JEV_LIVE=1` | Judge (TypeSafe Jev) | `TestJudgeLiveSmoke` |
| `CLAUDIA_CURSOR_LIVE=1` | Cursor Task + Session | `TestCursorTaskLiveSmoke`, `TestCursorSessionLiveSmoke`, `TestCursorSessionLiveSmokeSteer`, `TestCursorSavedSessionResumeLive`, `TestGoalJourneyLiveBackends/cursor`, `TestMCPLiveLoadAndSessionSeesMnemo/cursor`, `TestMCPHostLiveSeatsSeeMnemo/cursor`, `TestMCPExclusiveCursorSessionRoundTrip` |
| `CLAUDIA_OMP_LIVE=1` | Subscription sidecar + broker | `TestT865LiveNonBrokerCannotRead`, `TestT170LiveDirectKeychainACL`, `TestT865LiveBrokerRefreshPlans`, `TestT865LiveRebuiltBrokerRefused`, `TestT865LiveBrokerSmokeLaunchVerbs`, `TestT865LiveSidecarSurvivesJevonsdBounce`, `TestT195LiveSidecarSwitchKeepsConversation` |
| `CLAUDIA_RECOVERY_KEYCHAIN_LIVE=1` | Disposable Keychain recovery | `TestRecoverPlanDisposableKeychainAfterMissedRead` |

**Shared surfaces.** Some wires are not a backend — they are one
mechanism several backends run through, and the table above cannot
carry them: an agent that changed plan-usage fetching or the MCP
attach path would read six backend rows and find nothing about
what it touched. That is exactly the silence T100 was filed for, so
those surfaces get rows keyed by the surface, naming the gate that
un-skips each one.

| Surface | Gate | Must include |
|---------|------|--------------|
| Plan-usage fetching (`PlanUsage`, pacing, the direct-caller path) | `CLAUDIA_GROK_LIVE=1` | `TestGrokPlanUsageLive` — the only backend with a live plan-usage oracle; Claude, Codex and Cursor usage paths are hermetic-only residue |
| Goal continuation across backends | the gate of each backend you touched | `TestGoalJourneyLiveBackends` (per-backend subtests) |
| Broker seat grant, death and reclaim | the gate of each backend you touched, plus `CLAUDIA_NO_BROKER=0` and a running `claudia broker serve` | `TestBrokerReclaimLiveBackends` |
| MCP attach and host seats | the gate of each backend you touched | `TestMCPLiveLoadAndSessionSeesMnemo`, `TestMCPHostLiveSeatsSeeMnemo`, `TestMCPExclusiveSessionRoundTrip`, `TestMCPExclusiveCursorSessionRoundTrip` |
| The turn-silence bound (`turnSilenceBound`, `cursorPromptSilenceBound`) | `CLAUDIA_LIVE=1`, `CLAUDIA_GROK_LIVE=1`, `CLAUDIA_CURSOR_LIVE=1` | `TestT96MeasureSilenceClaude`, `TestT96MeasureSilenceGrok`, `TestT96MeasureSilenceCursor` — measurement probes that assert nothing, deliberately outside `make live`; run them by hand when the constant is questioned |
| MCP OAuth discovery against a real server | `CLAUDIA_MCP_OAUTH_LIVE=1` | `TestProbeMCPLiveAtlassian` — a vendor probe, deliberately outside `make live`; see `live-gate-exclusions.json` |

This table is not maintained by hand alone. `internal/livegate` reads
every live test out of the source under `make gate` and fails when one
is missing from either this table or `make live`'s `-run` expression —
so a live test added without a row here turns the hermetic gate red
(T100). A test that genuinely should not be in one of the two is
recorded, with its reason, in `live-gate-exclusions.json`.

```bash
# the backend you just changed
CLAUDIA_CODEX_LIVE=1 make live

# Session-wide change — all authed Session backends
CLAUDIA_LIVE=1 CLAUDIA_GROK_LIVE=1 CLAUDIA_CODEX_LIVE=1 CLAUDIA_CURSOR_LIVE=1 make live
```

Unset gates skip. CI never sets them. **You are the gate.**

`make live` is exclusive across checkouts on this host (🎯T88). Before
starting any backend tests it locks `$XDG_STATE_HOME/claudia/live.lock`
(or `~/.local/state/claudia/live.lock`). A concurrent invocation refuses
with `live gate busy` and a nonzero exit; it does not report a live RED.
The winner prints `wait 0s`, then holds the lock through the entire
`go test ./...` run. Do not remove the lockfile: an unlocked file is
normal, and deleting it while held defeats the lock. Direct env-gated
`go test` does not participate; use `make live` for the gate.

The tmux Session backend, its readiness verdicts and its paste-submit
framing were removed; every seat runs on the subscription sidecar.

**Done is not hermetic green.** Do not achieve a backend-behavior
target, and do not say the work is finished, until the live run is
green — or you have named the skip as residue (no binary, no auth).
A skipped live test is not a pass.

Not this rule: parser fixtures, capability-census tables, docs-only.

## Scratch files stay invisible to the toolchain

Your harness gives you a scratchpad outside this repo. Use it. When
something must live in the checkout, it goes under `_scratchpad/` —
never at the module root, never in a new plain-named directory.

The leading underscore is the whole mechanism: `go vet ./...` and
`go test ./...` skip directories whose names begin with `_` or `.`
(and `testdata`). `.gitignore` does nothing here — the go command does
not read it, so an ignored-but-plainly-named directory is still
compiled. A scratch copy of a library file at the module root declares
`package claudia` a second time and fails vet for every other seat:

```
vet: scratchpad/agent-head.go:62:11: undefined: Provider
```

That is a RED gate that is nobody's defect, and it cost a clean
full-suite citation on 2026-09-20 (T99).

Programs under `_scratchpad/` still build and run by explicit path —
`go run ./_scratchpad/tui-structure-exp` — but wildcards do not reach
them: `./_scratchpad/...` matches no packages. Name the package. A
scratch program carrying its own `go.mod` builds from inside its own
directory with `GOWORK=off`, because the parent `go.work` claims the
directory for this module; that is the workspace, not the underscore,
and it was true under the old name too.

Tree-walking tests use `internal/gowalk.IgnoredDir` rather than a
hand-kept list of directory names, so the suite reads exactly the files
the toolchain compiles. `scratch_invisible_test.go` pins the property:
it drops the incident's file at the scratch root and asserts the gate
stays green, with a throwaway module proving the same file in a
plainly-named directory still goes RED.

## Gates

profile: library
override:
  - pr-workflow: skip
  - ci-green: skip

The owner gate is local `make gate`. It attests the tree it passed on
(`.git/gate-attestation`, per clone, untracked); `scripts/hooks/pre-push`
builds, vets, and refuses a push unless that attestation names the tree of
the tip commit going out (earlier commits are not checked; the attestation
is never committed) — it does not re-run the gate, which takes ~25
minutes, longer than GitHub keeps an idle SSH connection.
There is no GitHub Actions CI; `make gate` and the pre-push hook are the CI. `make live` remains a
release-time owner gate for backend-behaviour changes. `/release` on
this repo: commit prep on master, `make gate` (and `make live` if the
diff touched a provider wire), `git push origin master`,
`gh release create`. No prep PR.

# claudia

Go library for embedding Claude, Grok, Codex, Bedrock, Ollama, and Cursor agents.
Consumer API: [`agents-guide.md`](agents-guide.md). This file is for
agents working *in* this repo.

```bash
make gate                      # hermetic owner gate (pre-push); same as CI
make live                      # real backends; each live env is opt-in
make supervisor-install        # host daemon under supervisord (evicts brew/launchd)
```

## Delivery

Owner ships to `master` by gated push (`make gate`, then
`git push origin master`). Ship only when asked. Do not open an owner
release-prep PR. Inbound PRs from others stay. After clone:
`make hooks` (or `git config core.hooksPath scripts/hooks`).
Flow: [docs/gate.md](docs/gate.md).

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
- app-server / ACP / exec / tmux paste-submit framing

**Run every backend whose wire you touched.** A Session-wide change
(Goal, `Send`, events) is every Session backend you can authenticate,
not just the one you had in mind. T39: hermetic journeys plus live
Grok and Codex were green while Claude hung on a multi-line
continuation that only the live TUI paste path could show.

**Backend wires.** Each row is one gate variable and the tests it
un-skips. If you touched the surface, the row is what you must run.

| Gate | Surfaces | Must include |
|------|----------|--------------|
| `CLAUDIA_LIVE=1` | Claude Task, Session and Pool | `TestTaskRunSmoke`, `TestClaudeTaskDisallowToolsLiveSmoke`, `TestModelObservableLive`, `TestModelNotFoundLiveFailLoud`, `TestAgentReadinessSmoke`, `TestAgentReadinessFailureOnDeadProcess`, `TestAgentSendAndWaitForResponse`, `TestAgentMultiTurn`, `TestRunHelper`, `TestCrashSurvival`, `TestRewindSessionLive`, `daemon.TestRewindLive`, `daemon.TestGoalCompleteCheckLive`, `daemon.TestAcquireLive`, `TestPoolAgentEventsLive`, `TestAcquireColdAndReturn`, `TestAcquireDropKillsWindow`, `TestAcquireKeepAliveFor`, `TestAcquireConcurrentDifferentKeys`, `TestAcquireHeldWindowNotReused`, `TestAcquirePoolCapEviction`, `TestAcquireErrorPolicy`, `TestPoolCrashSurvival`, `TestGoalJourneyLiveBackends/claude`, `TestMCPLiveLoadAndSessionSeesMnemo/claude`, `TestMCPHostLiveSeatsSeeMnemo/claude` |
| `CLAUDIA_GROK_LIVE=1` | Grok Task + Session, xAI Realtime | `TestGrokTaskRunSmoke`, `TestGrokSessionLiveSmoke`, `TestGrokSessionLiveSmokeSteer`, `TestExclusiveGrokSessionResumeLive`, `TestGrokPlanUsageLive`, `grok.TestLiveConnect` (also needs `XAI_API_KEY`), `TestGoalJourneyLiveBackends/grok`, `TestMCPLiveLoadAndSessionSeesMnemo/grok`, `TestMCPHostLiveSeatsSeeMnemo/grok`, `TestMCPExclusiveSessionRoundTrip` |
| `CLAUDIA_CODEX_LIVE=1` | Codex Task + Session | `TestCodexTaskRunSmoke`, `TestCodexSessionLiveSmoke`, `codex.TestLiveCodexTaskRun`, `TestGoalJourneyLiveBackends/codex`, `TestMCPLiveLoadAndSessionSeesMnemo/codex`, `TestMCPHostLiveSeatsSeeMnemo/codex` |
| `CLAUDIA_BEDROCK_LIVE=1` | Bedrock Task | `TestBedrockTaskLiveSmoke` |
| `CLAUDIA_OLLAMA_LIVE=1` | Ollama Task | `TestOllamaTaskLiveSmoke` |
| `CLAUDIA_CURSOR_LIVE=1` | Cursor Task + Session | `TestCursorTaskLiveSmoke`, `TestCursorSessionLiveSmoke`, `TestCursorSessionLiveSmokeSteer`, `TestCursorSavedSessionResumeLive`, `TestGoalJourneyLiveBackends/cursor`, `TestMCPLiveLoadAndSessionSeesMnemo/cursor`, `TestMCPHostLiveSeatsSeeMnemo/cursor`, `TestMCPExclusiveCursorSessionRoundTrip` |

**Shared surfaces.** Some wires are not a backend — they are one
mechanism several backends run through, and the table above cannot
carry them: an agent that changed plan-usage fetching or the tmux
paste-submit path would read six backend rows and find nothing about
what it touched. That is exactly the silence T100 was filed for, so
those surfaces get rows keyed by the surface, naming the gate that
un-skips each one.

| Surface | Gate | Must include |
|---------|------|--------------|
| Plan-usage fetching (`PlanUsage`, pacing, the direct-caller path) | `CLAUDIA_GROK_LIVE=1` | `TestGrokPlanUsageLive` — the only backend with a live plan-usage oracle; Claude, Codex and Cursor usage paths are hermetic-only residue |
| Goal continuation across backends | the gate of each backend you touched | `TestGoalJourneyLiveBackends` (per-backend subtests) |
| Broker seat grant, death and reclaim | the gate of each backend you touched, plus `CLAUDIA_NO_BROKER=0` and a running `claudia broker serve` | `TestBrokerReclaimLiveBackends` |
| tmux paste-submit framing for large payloads | `CLAUDIA_LIVE_SEND=1` | `TestT30LargePayloadSubmitsOnRealPath` — its own gate because it spends a turn on a 6400-byte send; `CLAUDIA_LIVE=1` alone does not un-skip it |
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

The owner gate is local `make gate`, run by `scripts/hooks/pre-push`.
Do not wait on `.github/workflows/test.yml`. `make live` remains a
release-time owner gate for backend-behaviour changes. `/release` on
this repo: commit prep on master, `make gate` (and `make live` if the
diff touched a provider wire), `git push origin master`,
`gh release create`. No prep PR.

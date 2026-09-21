# claudia

Go library for embedding Claude, Grok, Codex, Bedrock, Ollama, and Cursor agents.
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
| `CLAUDIA_LIVE=1` | Claude Task, Session and Pool | `TestTaskRunSmoke`, `TestClaudeTaskDisallowToolsLiveSmoke`, `TestModelObservableLive`, `TestModelNotFoundLiveFailLoud`, `TestAgentReadinessSmoke`, `TestAgentReadinessFailureOnDeadProcess`, `TestAgentSendAndWaitForResponse`, `TestT110PastedBriefIsActedOnLive`, `TestAgentMultiTurn`, `TestRunHelper`, `TestCrashSurvival`, `TestRewindSessionLive`, `daemon.TestRewindLive`, `daemon.TestGoalCompleteCheckLive`, `daemon.TestAcquireLive`, `TestPoolAgentEventsLive`, `TestAcquireColdAndReturn`, `TestAcquireDropKillsWindow`, `TestAcquireKeepAliveFor`, `TestAcquireConcurrentDifferentKeys`, `TestAcquireHeldWindowNotReused`, `TestAcquirePoolCapEviction`, `TestAcquireErrorPolicy`, `TestPoolCrashSurvival`, `TestGoalJourneyLiveBackends/claude`, `TestMCPLiveLoadAndSessionSeesMnemo/claude`, `TestMCPHostLiveSeatsSeeMnemo/claude` |
| `CLAUDIA_GROK_LIVE=1` | Grok Task + Session, xAI Realtime | `TestGrokTaskRunSmoke`, `TestGrokSessionLiveSmoke`, `TestGrokSessionLiveSmokeSteer`, `TestExclusiveGrokSessionResumeLive`, `TestGrokPlanUsageLive`, `grok.TestLiveConnect` (also needs `XAI_API_KEY`), `TestGoalJourneyLiveBackends/grok`, `TestMCPLiveLoadAndSessionSeesMnemo/grok`, `TestMCPHostLiveSeatsSeeMnemo/grok`, `TestMCPExclusiveSessionRoundTrip` |
| `CLAUDIA_CODEX_LIVE=1` | Codex Task + Session | `TestCodexTaskRunSmoke`, `TestCodexTaskGitWriteLiveSmoke`, `TestCodexSessionLiveSmoke`, `TestCodexGitWriteLiveSmoke`, `codex.TestLiveCodexTaskRun`, `TestGoalJourneyLiveBackends/codex`, `TestMCPLiveLoadAndSessionSeesMnemo/codex`, `TestMCPHostLiveSeatsSeeMnemo/codex`, `TestMCPCodexSeatCallsToolLive` |
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
| A pasted brief is acted on, not refused as foreign paste (the typed attribution line, `pasteAttribution`) | `CLAUDIA_LIVE=1` | `TestT110PastedBriefIsActedOnLive` — the seat writes a sentinel file named in a brief that reached it wrapped in `<pasted_content>`; the oracle is what the model DID, which no pane frame or hermetic fake can decide |
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

**Host load is a first-class variable for the Claude row.** The Claude
gate drives a real TUI through tmux, so it is the one row whose result
depends on how busy this machine is. Measured on the live path at load
average ~200 (🎯T101, 2026-09-20):

| Step | Latency |
|------|---------|
| `Start` → `WaitReady` (composer drawn) | **16.6s** |
| `send-keys -l` → the text echoed in the composer | up to **2.4s** |
| `Enter` → the first spinner frame | **1.6s** |

Cold readiness at higher load, measured by `cmd/t108ready` (spawn to a
live composer, no prompt submitted; 🎯T108, 2026-09-21). Load is the
1-minute average at spawn and at ready:

| Load | Spawn → live composer | Longest unchanged pane before live |
|------|------------------------|------------------------------------|
| 200–249 (5 seats) | 15.6s – 23.3s | 5.7s |
| 250–480 (8 seats) | 34.3s – **63.7s** | 12.5s |
| 583 (1 seat) | never: splash held for 126s of a 180s probe | — |

At every load the pane is blank until Claude Code paints its banner and
composer in one frame; most of the latency is before first paint.
`readyOverallTimeout` is **2m**, 1.9x the worst healthy sample. It was
30s, which refused all eight seats above 250.

Read those tables before blaming the submit or readiness path. A
readiness timeout names what the whole wait saw, not only its last
frame:

| Token | Meaning | Whose problem |
|-------|---------|---------------|
| `still_drawing` | pane changed within 30s of the bound, or a composer was drawn | the host |
| `not_started` | pane blank for the whole wait, process alive | the host, if load is high |
| `splash` / `rc_connecting` | composer drawn, not yet live | the host |
| `window_gone` | tmux lost the window mid-startup; the wait ends at once | claude exited or was killed |
| `no_composer` | pane drew something, then sat unchanged 30s+ with no composer ever | a defect: another screen |

Live Claude turns are what this fleet runs on, so load is largely
self-inflicted — `uptime` before the run, and record the number next to
the result.

🎯T101 is why these numbers are here. The submit path used to allow the
pane two samples, 800ms, to show either the payload or turn chrome, and
refused the send as `composer empty after paste (brief never reached
pane)` on the second empty frame — for turns the model went on to run.
Sub-400-byte messages take the typed branch and were the only ones
affected, which is why `TestAgentSendAndWaitForResponse` ("respond with:
ok", 16 bytes) failed while briefs of real size pasted fine. Fixed by
`submitEvidenceTimeout` and by the typed branch taking the same `landed`
evidence the paste branch takes; pinned by `TestT101*` in
`internal/tmuxagent`, against verbatim frames in
`testdata/frame_t101_*.txt`.

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
Do not wait on `.github/workflows/test.yml`. `make live` remains a
release-time owner gate for backend-behaviour changes. `/release` on
this repo: commit prep on master, `make gate` (and `make live` if the
diff touched a provider wire), `git push origin master`,
`gh release create`. No prep PR.

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

| Gate | Surfaces | Must include |
|------|----------|--------------|
| `CLAUDIA_LIVE=1` | Claude Task + Session | `TestAgentSendAndWaitForResponse`, `daemon.TestRewindLive`, `daemon.TestGoalCompleteCheckLive`, `daemon.TestAcquireLive`, `TestPoolAgentEventsLive`, `TestClaudeTaskDisallowToolsLiveSmoke`, `TestGoalJourneyLiveBackends/claude`, `TestMCPLiveLoadAndSessionSeesMnemo/claude`, `TestMCPHostLiveSeatsSeeMnemo/claude` |
| `CLAUDIA_GROK_LIVE=1` | Grok Task + Session | `TestGrokSessionLiveSmoke`, `TestGrokSessionLiveSmokeSteer`, `TestGoalJourneyLiveBackends/grok`, `TestMCPLiveLoadAndSessionSeesMnemo/grok`, `TestMCPHostLiveSeatsSeeMnemo/grok`, `TestMCPExclusiveSessionRoundTrip` |
| `CLAUDIA_CODEX_LIVE=1` | Codex Task + Session | `TestCodexSessionLiveSmoke`, `TestGoalJourneyLiveBackends/codex`, `TestMCPLiveLoadAndSessionSeesMnemo/codex`, `TestMCPHostLiveSeatsSeeMnemo/codex` |
| `CLAUDIA_BEDROCK_LIVE=1` | Bedrock Task | `TestBedrockTaskLiveSmoke` |
| `CLAUDIA_OLLAMA_LIVE=1` | Ollama Task | `TestOllamaTaskLiveSmoke` |
| `CLAUDIA_CURSOR_LIVE=1` | Cursor Task + Session | `TestCursorTaskLiveSmoke`, `TestCursorSessionLiveSmoke`, `TestCursorSessionLiveSmokeSteer`, `TestGoalJourneyLiveBackends/cursor`, `TestMCPLiveLoadAndSessionSeesMnemo/cursor`, `TestMCPHostLiveSeatsSeeMnemo/cursor`, `TestMCPExclusiveCursorSessionRoundTrip` |

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

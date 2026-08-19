# claudia

Go library for embedding Claude, Grok, Codex, Bedrock, and Ollama agents.
Consumer API: [`agents-guide.md`](agents-guide.md). This file is for
agents working *in* this repo.

```bash
go test -race -count=1 ./...   # hermetic default; CI runs this
make live                      # real backends; each gate is opt-in
```

## Delivery

Merged to default branch (`master`). Ship only when asked (`/push`,
“open a PR”, “release”).

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
- MCP attach (`LoadMCP`, `EnsureMCP`, `Config.MCPServers`)
- app-server / ACP / exec / tmux paste-submit framing

**Run every backend whose wire you touched.** A Session-wide change
(Goal, `Send`, events) is every Session backend you can authenticate,
not just the one you had in mind. T39: hermetic journeys plus live
Grok and Codex were green while Claude hung on a multi-line
continuation that only the live TUI paste path could show.

| Gate | Surfaces | Must include |
|------|----------|--------------|
| `CLAUDIA_LIVE=1` | Claude Task + Session | `TestAgentSendAndWaitForResponse`, `TestGoalJourneyLiveBackends/claude`, `TestMCPLiveLoadAndSessionSeesMnemo/claude` |
| `CLAUDIA_GROK_LIVE=1` | Grok Task + Session | `TestGrokSessionLiveSmoke`, `TestGoalJourneyLiveBackends/grok`, `TestMCPLiveLoadAndSessionSeesMnemo/grok` |
| `CLAUDIA_CODEX_LIVE=1` | Codex Task + Session | `TestCodexSessionLiveSmoke`, `TestGoalJourneyLiveBackends/codex`, `TestMCPLiveLoadAndSessionSeesMnemo/codex` |
| `CLAUDIA_BEDROCK_LIVE=1` | Bedrock Task | `TestBedrockTaskLiveSmoke` |
| `CLAUDIA_OLLAMA_LIVE=1` | Ollama Task | `TestOllamaTaskLiveSmoke` |

```bash
# the backend you just changed
CLAUDIA_CODEX_LIVE=1 make live

# Session-wide change — all authed Session backends
CLAUDIA_LIVE=1 CLAUDIA_GROK_LIVE=1 CLAUDIA_CODEX_LIVE=1 make live
```

Unset gates skip. CI never sets them. **You are the gate.**

**Done is not hermetic green.** Do not achieve a backend-behavior
target, and do not say the work is finished, until the live run is
green — or you have named the skip as residue (no binary, no auth).
A skipped live test is not a pass.

Not this rule: parser fixtures, capability-census tables, docs-only.

# Cursor Provider Oracle Map

Status: verification plan for `ProviderCursor`.

Cursor provider work harnesses the **Cursor Agent CLI** (`agent` /
`cursor-agent`), not the TypeScript/Python Cursor SDK.

Public contracts:

- Persistent Session: `agent acp` (ACP JSON-RPC)
- Task: `agent --print --output-format stream-json` (NDJSON; camelCase usage)

| Target | Verification Class | Machine Oracle | Live Role |
| --- | --- | --- | --- |
| Provider seams | New-code lifecycle seam | Backend dispatch selects `cursorAgentBackend` + `cursorTaskBackend` | None |
| Binary discovery | Deterministic resolver | `TestResolveCursorBin` injects env, prefers `cursor-agent`, skips `~/.grok/bin/agent`, falls back to `~/.local/bin/agent` | Optional install sanity |
| Session contract | ACP fixtures + docs | `docs/cursor-acp-session.md`, `testdata/cursor/acp/fake_acp.py` | Optional live handshake |
| Session mode | Sidecar dispatch + in-memory watch | Product `Start` is the sidecar. Concrete `StartAgent` returns `ErrLegacyCLISeatTransport` (`TestT1055CLIBackendsDoNotSpawn`). Opening silence is `TestT1055CursorOpeningWatchReportsStuck`. | `CLAUDIA_CURSOR_LIVE` session smoke on product `Start` |
| Task mode | Fake print stream | `TestHermeticCursorTaskRun`, `TestParseCursorTaskLine*`, `TestCursorTaskArgsResumeAndModel` | `TestCursorTaskLiveSmoke` |
| Resume fail-closed | Capability and registry | CLI load hermetics that started a seat are removed. Registry cursor launch through the concrete backend refuses (`TestT1055RegistryCursorLaunchRefusesCLI`). | `TestCursorSavedSessionResumeLive` on product `Start` |
| Permissions / extensions | Precheck | `cursorSessionPrecheck` via the capability audit. The hyphen-option and ask-question seat tests started a CLI and are removed. | None |
| Capability gaps | Negative capability oracle | `TestCursorCapabilityMatrixIsExplicit`, Rewind fail-closed | Human review of accepted gaps |
| Hermetic MCP | Project mcp.json + ACP | `TestEnsureAndLoadCursorMCP`, `TestWriteExclusiveCursorProjectMCPWritesOnlyNamedServers`, `TestMCPExclusiveCursorProjectMCPJourney` (no HOME rewrite) | `TestMCPLiveLoadAndSessionSeesMnemo/cursor`, `TestMCPExclusiveCursorSessionRoundTrip` |
| Plan usage | Fixture parser | `TestParseCursorPeriodUsage*`, opt-in off, HTTP hermetic | Optional dashboard RPC |

## Fault Checks

- Load failure + `RequireResume` must not mint.
- Hyphenated permission option ids must be selected from the offered list.
- `cursor/ask_question` must not stall `WaitForResponse`.
- Missing/out-of-range `totalPercentUsed` is unavailable, never 0%.
- Task result usage maps camelCase (`inputTokens`, …) into `Usage`.

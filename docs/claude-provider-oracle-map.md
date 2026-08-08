# Claude Provider Oracle Map

Status: active verification plan for 🎯T11 / T11.1 / T11.2.

`ProviderClaude` is the **default** runtime. It is CLI-backed:

| Mode | Process model | Public contract |
| --- | --- | --- |
| **Task** | New `claude` per prompt | `claude -p --output-format stream-json …` |
| **Session** | Persistent PTY in tmux | `claude` in a claudia tmux window + `~/.claude/projects/…/*.jsonl` tail |

Live Claude runs (`CLAUDIA_LIVE=1`) are smoke/regression only. Targets
retire when hermetic fixtures, fakes, and fault checks prove the mapping.
Live residual is declared below, never used as the sole achievement
evidence.

Public capability claim (`claudeProviderCapabilities`):

| Capability | Claimed |
| --- | --- |
| Task | yes |
| Session | yes |
| Resume | yes |
| Rewind | yes |
| Cost | yes (Task per-prompt; Session cumulative usage, not USD) |
| Permissions | yes (`PermissionMode`, `DisallowTools`) |
| TmuxAttach | yes |
| TerminalBytes | yes |

## Claim → oracle matrix

| Claim (README / agents-guide / API) | Verification class | Machine oracle | Live role | Status |
| --- | --- | --- | --- | --- |
| Default provider is Claude when `Provider` empty | Lifecycle seam | `TestTaskBackendForProvider`, empty-provider agent backend selects Claude | None | Hermetic |
| Binary discovery (`CLAUDE_BIN`, PATH, install dirs) | Deterministic resolver | `TestResolveClaudeBin` family in `task_test.go` | Optional install sanity | Hermetic |
| Task start / stream-json spawn | Fake CLI process | `TestHermeticTaskRunClaudeSpawn` + golden `testdata/claude/exec/success.jsonl` | `CLAUDIA_LIVE=1` `TestTaskRunSmoke` | Hermetic |
| Task text + result + cost/usage | Fixture parser + spawn | `TestParseTask*` + hermetic spawn asserts `CostUSD` / `Usage` | Live smoke | Hermetic |
| Task tool_use events | Fixture + spawn | `TestParseTaskAssistantToolUseEvent` + `TestHermeticTaskRunClaudeToolUse` | Live (tools spend credit) | Hermetic |
| Task error events | Fixture + spawn | `TestHermeticTaskRunClaudeErrorEvent` + parse tests | None | Hermetic |
| Task resume via `TaskConfig.ClaudeID` → `--resume` | Fake CLI argv oracle | `TestHermeticTaskRunClaudeResumeArgs` | Live resume smoke residual | Hermetic |
| Task Cancel → SIGINT | Slow fake + signal | `TestHermeticTaskCancelSendsSIGINT` | Live residual | Hermetic |
| Task Stop → permanent, cancels in-flight | Slow fake + context | `TestHermeticTaskStopCancelsInFlight` | Live residual | Hermetic |
| Session Start / Send / Interrupt / Resize / Stop (ops) | Injected agent backend | `TestStartUsesInjectedBackendLifecycle` (`fake-claude`) | `CLAUDIA_LIVE=1` agent smoke | Hermetic ops; live for real TUI |
| Session readiness (capture-pane) | Live / tmux | `TestAgentSmoke` (gated) | Required for real TUI timing | **Live residual** |
| Session stream events from JSONL | Hermetic tail | `TestHermeticClaudeSessionJSONLTail` (TailJSONL + write transcript) | Live multi-turn | Hermetic |
| Session WaitForResponse settle / tool_use non-terminal | Synthetic events | `TestWaitForResponse*` in `agent_test.go` | Live multi-turn | Hermetic |
| Session resume: JSONL exists → `--resume` flag path | HOME redirect + backend request | `TestHermeticClaudeSessionResumeDecision` | Live resume | Hermetic |
| Session fail-closed `RequireResume` when JSONL missing | Start path | `TestHermeticClaudeRequireResumeFailsClosed`, `TestHermeticMaterializedRequireResumeFailsClosed` | — | Hermetic (fix) |
| Materialized only after conversation evidence (not bare Start) | Registry Launch / MarkMaterialized | `TestHermeticLaunchDoesNotMaterializeWithoutJSONL`, `TestHermeticMaterializeFromJSONLAndRequireResume` | — | Hermetic (fix) |
| Transcript path / SessionExists durability | Path + HOME redirect | `TestSessionJSONLPath`, `TestSessionExists` | Live file ownership residual | Hermetic |
| Rewind JSONL turn boundaries + tool_use safety | Metamorphic fixture | `TestRewindJSONL*` | `TestRewindSessionLive` residual | Hermetic core; live end-to-end residual |
| Permissions / disallow tools always applied | Arg construction + Task spawn | Session Start request carries disallowed list; Task always passes `--disallowedTools` including `BaseDisallowedTools` | Live tool denial residual | Hermetic list; live residual for real CLI enforcement |
| Cost capability claim | Capabilities struct + Task result | `claudeProviderCapabilities().Cost` + result fixture cost fields | Live billing residual | Hermetic for Task USD; Session has usage not USD (documented) |
| Tmux attach / terminal bytes | Ops + SubscribeTerminal | Fake lifecycle routes terminal bytes; attach command shape | Live `tmux attach` residual | Hermetic ops; live attach residual |
| Pool / Registry participation | Unit + live pool | Registry persistence tests; pool live-gated | Pool acquire live residual | Registry hermetic; pool mostly live |
| Codex/Grok gaps stay fail-closed (Claude contrast) | Negative capability | Codex session experimental; Grok rewind unsupported | None | Hermetic |

## Required golden fixtures

`testdata/claude/exec/`:

- `success.jsonl` — system/init (session id), assistant text, result with cost/usage.
- `error.jsonl` — init + result subtype error.
- `tool_use.jsonl` — init, assistant tool_use (+ optional text), result.
- (resume/cancel use the same fixtures or a slow shell wrapper; no extra golden required for signal tests.)

Fixtures must stay small, redacted, and hand-owned.

## Fault checks (must fail when broken)

Before 🎯T11 is retired, tests prove these injected faults fail:

- Dropped session id on init → ClaudeID not set / oracle fails.
- Wrong final-message selection → LastResult / result content mismatch.
- Silent unsupported capability success for *other* providers only (Claude claims full matrix; Codex/Grok fail-closed).
- **RequireResume with missing JSONL succeeds** → must fail closed (no `--session-id` mint).
- Private-storage shortcut for rewind: Claude *is* allowed to rewrite its own JSONL under `~/.claude/projects` (that is the public Claude Code resume model). Non-Claude providers must not use Claude rewind rules.

## Human / live residue

Machine checks do not certify:

- Real Anthropic auth, model quality, or end-to-end tool execution cost.
- TUI readiness timing under load (`detectReady` 30 s cap, capture-pane regex).
- Orphan process / tmux window leak under hostile kill of the *consumer* (tmux substrate is designed for crash-survival; full orphan reaping is broker 🎯T2 territory).
- Whether Session mode “cost” should ever expose USD (today: cumulative token usage only).
- Jevons-side product gaps (chat UI, fleet registry UX, interrupt button wiring) — see below.

Live gates (optional smoke, never sole retirement evidence):

| Env | What it covers |
| --- | --- |
| `CLAUDIA_LIVE=1` | `TestTaskRunSmoke`, `TestAgentSmoke` / multi-turn / pool / `TestRunHelper`, `TestRewindSessionLive` |

## Jevons-side gaps (do not patch from claudia)

**Re-stitch audit (🎯T11.1, 2026-08-03):** see **`docs/claude-jevons-restitch.md`** for WORKS/GAP/UNKNOWN rows after Grok-only production wiring. Selection surface is jevons 🎯T148 (achieved). Default fleet remains **Grok** (intentional); Claude is opt-in via provider select.

List for overseer → **jevons-po** (claudia owns runtime contract only):

- Confirm Jevons fleet threads use `ProviderClaude` when selected (default fleet is Grok — restitch map) and surface `CapabilityError` if a consumer ever selects Codex Session / Grok Rewind.
- Interrupt / stop / rewind UI must call claudia `Agent.Interrupt` / `Stop` / `Rewind` (or Task Cancel/Stop) — product wiring is Jevons-owned.
- Transcript durability after Jevons process death relies on claudia JSONL + Registry `Materialized` / `RequireResume`; Jevons must persist `SessionID` and not mint a new id on reconnect. Jevons `SessionsDir` / transcript reader remain Grok-tree (`~/.grok/sessions`) — Claude JSONL inspect is a jevons gap (restitch J2).
- Overseer MCP is registered only via Grok user-scoped config (`ensureGrokMCPServer`) — Claude overseer would start toolless (restitch J1).
- Any Jevons assumption that Session mode reports per-turn USD cost is wrong — Task has `CostUSD`; Session has `Usage()` tokens only.

## Relationship to other maps

- Codex: `docs/codex-provider-oracle-map.md` (🎯T4)
- Grok: `docs/grok-provider-oracle-map.md` (🎯T7)
- Broker fake-claude harness: `docs/broker-oracles.md` (AIMD/cost/reap — separate from public ProviderClaude surface)

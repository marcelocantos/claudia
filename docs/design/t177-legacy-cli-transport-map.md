# 🎯T177 scout: claudia's legacy CLI seat transport, mapped

Scout report for 🎯T177 (companion to jevons 🎯T1055). This maps every
place claudia still offers a seat transport other than the Oh My Pi
(OMP) sidecar, what depends on each piece today, and what has no
sidecar equivalent. It removes nothing. Line numbers are against
`bd19d57` (master, 2026-10-10).

## 1. Vocabulary and scope

A **seat** is a Session-mode agent: `Start` / `Adopt` / `Registry.Launch`
/ a broker grant. **Task mode** (`Run`, `Task.Run`, `task_run`) is one
process per prompt and is not a seat; it is listed in §9 as a scope
question, not inventoried as transport.

claudia has five Session backends. One is the sidecar; four are
legacy CLI process transports:

| Backend | Selected for `Config.Provider` | Process | Status today |
|---|---|---|---|
| `ompAgentBackend` (`omp_agent.go`) | `anthropic`, `openai-codex`, `xai-oauth`, `cursor`, `grok` | bun sidecar over `omp.sock` | the fleet transport |
| `claudeAgentBackend` (`agent.go:1080`) | `""`, `claude` | `claude` TUI in a tmux window | **legacy, reachable** |
| `codexAgentBackend` (`agent.go:1316`) | `codex` | `codex app-server` JSON-RPC over stdio | **legacy, reachable** |
| `grokAgentBackend` (`agent.go:1412`) | `grok` | `grok` ACP stdio, or connect-mode serve | legacy, **unreachable** from `Start` (grok now routes to OMP) |
| `cursorAgentBackend` (`cursor_acp.go:1129`) | `cursor` | `agent acp` stdio | legacy, **unreachable** from `Start` (cursor routes to OMP) |

The fork is two functions:

- `useOMP` / `ompProviderID` (`omp_agent.go:52-69`): `anthropic`,
  `openai-codex`, `xai-oauth`, `cursor` and `grok` go to the sidecar.
  **`claude` and `codex` are absent from the switch**, so those two
  ids, and the empty provider, fall through.
- `agentBackendForProvider` (`agent.go:573-588`): the fall-through.
  `""`/`claude` → tmux, `codex` → app-server, `grok`/`cursor` → ACP
  (dead branches by dispatch, see §4), `bedrock` → capability refusal.

`agentBackendFor` is called from `Start` (`agent.go:728`, `:798`),
`Migrate` (`migrate.go:333`) and the broker backend's capability report
(`broker_agent.go:101`). `Adopt` (`agent.go:1148`) hard-codes
`adoptClaudeBackend`. The registry's restart path (`registry.go:587-600`)
has its own per-transport switch (§5).

So the whole legacy surface is reachable through exactly one gap: a
provider id of `""`, `claude` or `codex` arriving at `Start`, `Adopt`,
`Launch` or `Migrate`. jevons 🎯T1047 is the live specimen of that gap.

## 2. What the live system is doing right now (2026-10-10)

Measured on this host, not inferred from code:

| Observation | Value |
|---|---|
| tmux windows on the claudia server (`~/.local/state/claudia/tmux.sock`) | 2: the anchor shell `@0`, and `@114 claudia-71fc7dae` |
| What `@114` is | this scout seat (`cl-t177-legacy-tmux-transport-scout`, provider unset in `agents.json`) |
| jevons `~/.jevons/agents.json` rows by provider | 31 `openai-codex`, 7 `anthropic`, 1 empty (this scout) — 38 of 39 on the sidecar |
| claudia `grants.json` rows by provider | 36 `anthropic`, 22 `cursor`, 18 `openai-codex`, **14 `claude`**, 3 `grok`, 3 `xai-oauth`, 1 empty |
| `claude`/`grok` grant rows that are `auto_start` | 14 `claude` + 2 `grok`; none has a live window or process today |
| Sidecar seat stores under `~/.local/state/claudia/omp-seats` | 270 |
| Daemon log since 2026-10-05: `agent started/adopted` by provider id | 135 `anthropic`, 113 `openai-codex`, 32 `claude`, 16 `grok`, 7 `codex`, 5 `xai-oauth`, 1 `cursor` |
| Last `provider=claude` start | 2026-10-10 15:26 `jv-t1014-anthropic-omp-probe` (the 🎯T1047 incident) |
| `provider=claude` starts on 10-07/10-08 | `arrai-t4x-*` (4), `jv-t102x-*` (5), `pimp-*` (5), `trustprobe-*` (5) |
| `provider=codex` starts on 2026-10-10 | 7 (`jv-t1032-sandbox-migrate`, `jv-t1046-*` ×3, `jv-t603-gate-load`, `jv-t862.*` ×2) |

Reading: the fleet is already almost entirely on the sidecar; the last
tmux seat is the scout writing this. But the daemon launched `claude`
rows as recently as today and `codex` rows seven times today, so the
CLI ids are still arriving at `Launch`. Where from is a 🎯T1055
question (§8, fog-unknown 1). The 14 `claude` and 2 `grok` `auto_start`
grant rows are leftovers that a daemon restart would try to adopt or
relaunch through tmux / the registry's grok branch (jevons already
calls these "not that fleet", `internal/seatreg/remint.go:44`).

## 3. Inventory A: the tmux Claude Session path

This is the transport the owner's directive names. Sizes are whole
files; shared files are marked.

### 3.1 `internal/tmuxagent` (2,243 non-test lines, 3,800 test lines, 24 testdata frames)

| File | Lines | Role |
|---|---|---|
| `send.go` | 759 | typed vs paste submit, `landed` evidence, `submitEvidenceTimeout`, the 🎯T101/🎯T110 attribution line |
| `readiness.go` | 673 | `WaitReady`, `MatchReady`, `NotReadyReason` tokens, trust-dialog walk (`8b60b65`) |
| `control.go` | 211 | tmux control-mode client (`DialControl`) |
| `window.go` | 192 | `SpawnWindow`, `KillWindow`, window options (`@claudia-session-id`, `claudia-held`, `claudia-deadline`) |
| `server.go` | 150 | dedicated server, `claudia-anchor` session, `CLAUDIA_TMUX_SOCKET` |
| `lifecycle.go`, `list.go`, `session.go`, `claude_markers.go` | 258 | window listing, `SessionWindowName`, Claude frame markers |

Importers outside the package: `agent.go`, `pool.go`, `registry.go`,
`goal.go`, `turn_wait.go`, `task.go` (one reference), the six probe
commands (§3.4), `internal/broker/brokertest`, `internal/testctlenv`,
and `daemon/imports_test.go` (which *forbids* the daemon from importing
it).

### 3.2 Root package, tmux-only or tmux-first

| Site | What it is |
|---|---|
| `agent.go:1080-1197` | `claudeAgentBackend.StartAgent`, `Adopt`, `adoptClaudeBackend`, `attachClaudeWindow`, `ErrNoSessionWindow` |
| `agent.go:434` `checkTmux`; `agent.go:251-262, 323-327` `tmuxWindowID`, `tmuxCtrl`, `windowAliveFn` | handle fields the sidecar leaves nil |
| `agent.go:1626` `WindowID`, `:1670` `TermLogPath`, `:1682` `AttachCommand` | human-observability API; sidecar seats return empty |
| `agent.go:374-385` terminal log (`termLog`, `TerminalBytes` capability) | raw PTY bytes to a file; tmux-only |
| `pool.go` (589) | warm pool: `Acquire`, `AcquireDirect`, `spawnPoolWindow`, `claudia-pool-*` windows; **tmux-only, no jevons caller found** |
| `rewind.go` (172), `registry_rewind.go` (71), `agent.go:2406` `Rewind` | JSONL truncation; `CapabilityRewind` is Claude-only (`capability.go:651`) |
| `tui_preview.go` (438) | pane-frame → Markdown preview; **shared**: the sidecar emits `PreviewUpdateAppend` (`omp_agent.go:465`) |
| `turn_wait.go:107-115` | window-liveness probe cadence (`windowCheckTTL`); shared file |
| `goal.go:238` | paste-chip sizing comment; shared file |
| `registry.go:835` `isClaudeProvider`; `:587-600` adopt switch; `:1027-1037` `reapSessionWindows` | the registry knows the transport by provider id |
| `broker_agent.go:478-489` `repoint` | copies `windowID` on a broker re-point |
| `daemon/daemon.go:907-910, 968-971, 1447-1455, 1502` | grant status carries `WindowID`, `TermLogPath`, `AttachCommand` |
| `daemon/daemon.go:1029` `procProvider`, `:1732` `admitTask` | empty provider normalises to `claude` |
| `internal/broker/wire_grants.go:366-369, 671-705` | `window_id`, `attach_command` on the grant wire |
| `internal/broker/wire.go:220-245` `SpawnRequest.Validate` | the older spawn verb: "this broker spawns `claude` only" |
| `capability.go:30-32, 147, 184, 235, 262, 306, 348, 404, 627-656` | `CapabilityTmuxAttach`, `TerminalBytes`, `Rewind` claims, and seven refusal reasons that explain themselves in terms of tmux |
| `README.md` (21 mentions), `agents-guide.md` (23; §"tmux substrate" at line 1356, "Human observability: AttachCommand" at 1365) | consumer docs |
| `AGENTS.md` here: the host-load tables (🎯T101, 🎯T108), the readiness-token table, the live Claude row | repo doctrine written for the tmux path |

### 3.3 Test corpus coupled to tmux (≈9,400 lines)

Root: `agent_test.go` (1,151), `pool_test.go` (520),
`claude_provider_hermetic_test.go` (489), `turn_wait_test.go` (330),
`registry_t34_test.go` (272), `goal_test.go` (264),
`pool_events_test.go` (262), `goal_live_test.go` (231),
`agent_crash_test.go` (189), `agent_send_t30_live_test.go` (189),
`pool_events_live_test.go` (134), `live_tmux_test.go` (123),
`agent_send_t110_live_test.go` (105), `pool_clock_test.go` (95),
`agent_send_submit_test.go` (94), `t602_alive_test.go` (92),
`testctlenv_leak_test.go` (69), `t601_prompt_in_flight_test.go` (40).
Also `daemon/pool_test.go` (232), `daemon/pool_live_test.go` (98),
`daemon/goal_live_test.go` (115), all of `internal/tmuxagent/*_test.go`,
`internal/broker/brokertest/fakeclaude*.go` (293, a fake Claude TUI
whose frames are checked against `tmuxagent.MatchReady`), and
`internal/testctlenv` (245, the registry of test-control env vars that
exists *because* a tmux server freezes its environment).

26 root test files mention tmux; 51 test files across the module do.

### 3.4 Tools, CI, state

- Probe programs: `cmd/probe-ready-tmux`, `cmd/t108ready`,
  `cmd/t28repro`, `cmd/t28size`, `cmd/t30send`, `cmd/t284repro` — all
  tmux-only reproductions of past incidents.
- `.github/workflows/test.yml:22-44` installs tmux for the hermetic
  suite.
- `~/.local/state/claudia/tmux.sock` and the `claudia-anchor` session.

## 4. Inventory B–D: the other three CLI session backends

### B. Codex app-server (reachable: `codex` id falls through)

`agent.go:1316` `startCodexAgent`; `codex_app_server.go` (418),
`codex_session.go` (681), `codex_steer.go` (128), `codex_rollout.go`
(55); `codex_git_write.go` (190) and `codex_auth.go` (202) are shared
with Task mode and plan usage. `mcp_exclusive.go:67-120` builds the
per-thread `CODEX_HOME` isolate (`~/.local/state/claudia/codex-homes`).
Capability claims at `agent.go:594-600`. Live rows: `TestCodexSessionLiveSmoke`,
`TestCodexGitWriteLiveSmoke`, `TestGoalJourneyLiveBackends/codex`,
`TestMCPLiveLoadAndSessionSeesMnemo/codex`, `TestMCPHostLiveSeatsSeeMnemo/codex`,
`TestMCPCodexSeatCallsToolLive`.

jevons 🎯T1047 names `codex → openai-codex` as the sibling of the
`claude → anthropic` rewrite, which is why this backend is in scope
and not merely "another process-local transport".

### C. Grok ACP and connect-mode (unreachable from `Start`)

`ompProviderID(ProviderGrok)` returns `xai-oauth`, so `Start` never
reaches `grokAgentBackend`. What remains is dead by dispatch but still
compiled and still branched on:

- `grok_acp.go` (926), `grok_acp_connect.go` (251), `grok_home.go`
  (140), the Session half of `grok_spawn.go` (445, shared with Task),
  `acp_*.go` (484, shared with Cursor ACP), `agent.go:1412-1460`.
- Registry fields `ConnectURL`, `ConnectPID`, `GrokConnect`
  (`registry.go:113-121`) and the adopt branch at `registry.go:594`,
  which calls `startProc` — and `startProc` now dispatches to the
  sidecar, which ignores those fields. A `grok` row with a connect
  endpoint is therefore adopted onto a *different* transport silently.
- `~/.local/state/claudia/grok-serve`, `grok-homes`.
- `mcp_exclusive.go:17-64` process-private `GROK_HOME`.
- Live rows `TestGrokSessionLiveSmoke*`, `TestExclusiveGrokSessionResumeLive`,
  `TestGoalJourneyLiveBackends/grok`, the MCP grok subtests: by
  dispatch these already exercise the sidecar. Not re-verified live
  in this scout (fog-unknown 4).

### D. Cursor ACP session (unreachable from `Start`)

`cursor_acp.go` (1,202), `cursor_reap.go` (241); `cursor_models.go`
(295) and `cursor_task.go` (175) are shared with Task / model default.
`reapCursorACPDef` is still called from the registry adopt switch
(`registry.go:596-599`). Same dead-by-dispatch status as Grok.

## 5. Inventory E: code that exists to tell CLI seats from OMP seats

These are the places the directive calls "provider routing / rewrite
logic that exists only to preserve CLI identity" and "broker code that
distinguishes CLI vs OMP seats".

| Site | Mechanism |
|---|---|
| `provider.go:29-57` `PlanProvider` / `SubscriptionSeatProvider` | the two-way map between fleet ids (`claude`, `codex`, `grok`) and sidecar ids (`anthropic`, `openai-codex`, `xai-oauth`); keeps "legacy CLI identities readable" |
| `omp_agent.go:52-69` `useOMP`, `ompProviderID` | the fork; `claude` and `codex` are deliberately not mapped |
| `agent.go:60-69` `Config.OMP` | "retained on persisted grants", no longer consulted for launch (🎯T866.5); still copied on the wire (`broker_wire.go:236`, `registry_lifecycle.go:136`) |
| `registry.go:587-600` adopt switch | one branch per transport; `isClaudeProvider` chooses `adoptProc` |
| `migrate.go:303-349` | `CheckCapability(Migrate)` only for non-OMP sides; `capability.go:519-523` exempts every sidecar id from the matrix |
| `migration_transfer.go:225-227` | transfer seat is forced onto `SubscriptionSeatProvider(provider)` so a `claude` destination does not mint a tmux seat |
| `registry_migrate_stopped.go:103`, `migrate.go:316` | default model only for OMP destinations |
| `plan_fleet.go:21, 45-47`; `daemon/daemon.go:1715` | fleet id lists still enumerate `claude`, `codex`, `grok`, `cursor` |
| `internal/broker/wire.go:84-85, 238-243` | broker-level `ProviderClaude` normalisation |
| `cmd/claudia/broker_seat.go:67` `-adopt` | "prefer reattaching a provider process the daemon can find" |

After the purge the only honest shape is: `ompProviderID` is total over
the supported ids (including `claude` → `anthropic`, `codex` →
`openai-codex` as *aliases*, or the aliases are refused at the API
edge), and everything in this table collapses to that one function.

## 6. Dependencies outside this repo

### 6.1 jevons (owned by 🎯T1055; `jv-t1055-omp-only-purge-implement` is running)

Grouped by what breaks if claudia deletes the transport first:

**Breaks at build or boot**

- `internal/cli/agenttmux.go` + `cmd/jevonsd/main.go:164-173`: boot
  reconciles the claudia tmux server's global environment (🎯T282).
  Reaches the socket directly with `tmux -S`.
- `internal/cost/killswitch.go` `TmuxKillSwitch`, `cmd/jevonsd/cost.go:116-200`:
  the fleet kill-switch and orphan-burner scan list tmux windows by
  `@claudia-session-id`.
- `internal/panecensus` + `internal/mcpserver/pane_census.go` +
  `seat_observations.go:180-193`: pane census (🎯T459) and **flight
  inference from pane titles**. With no panes, the census is empty and
  `observePanePresence` contributes nothing; needs a sidecar-side
  presence source or deletion.
- `internal/upgrade/{handles,reattach,claude_guard,handoff_transport,upgrade}.go`:
  the upgrade handoff carries `TmuxWindowID`, probes pane PIDs, and
  preserves "a surviving legacy CLI process" across a daemon upgrade
  (`cmd/jevonsd/main.go:680-705`, `seatreg.RemintRegistryExcept`).
- `internal/fleet/claude_trust.go`, `internal/claudetrust`: the trust
  dialog pre-flight (a tmux seat has no human to answer it).
- `scripts/journey-suite/t579_tmux_socket.go` (149), `isolated_broker.go`,
  `mux_owner.go`, `j21_goal_continue.go`: journeys that assert seats
  run on a private tmux socket.
- `scripts/t796-live-throwaway/main.go:168`: starts
  `Config{Provider: ProviderClaude}` explicitly.

**Routing that becomes unconditional (the 🎯T1047 fix)**

- `internal/cli/provider.go:69-75` `SidecarLaunchProvider`;
  `internal/mcpserver/agents.go:1040-1060` `alignStartTransport` (only
  rewrites when `!existed || providerArg != ""`);
  `internal/seatreg/remint.go` `RemintSubscription`, `subscriptionPlan`
  (lists both id families); `cmd/jevonsd/main.go:685-705` preserved-CLI
  handoff.

**Vocabulary only (string lists naming `claude`/`codex`/`grok`)**

`internal/server/model_company.go:25`, `internal/treeguard/coverage.go:38`,
`internal/harnessusage/report.go:48`, `internal/spool/spool.go:29`,
`internal/server/agent_auth_recover.go:34,278`, `internal/fleet/reply.go:77`,
`internal/mcpserver/t890_stall_fallback.go:36`, `internal/mcpserver/send_claim.go:86`
(`submitUnverifiedMarkers` are the tmux submit loop's error strings),
`internal/mcpserver/t745_busy_pane.go`, `turn_flight.go`,
`seat_waits.go`, `internal/escalate/escalate.go`,
`docs/fleet-census.md`, `docs/architecture-current.md`.

### 6.2 Other fleets on the same broker

`arrai-*` and `pimp-*` seats started with `provider=claude` on
2026-10-07/08 (daemon log). These are jevons-managed fleets for other
repos (an `arrai-po` sidecar seat store exists), so they inherit the
🎯T1055 fix; no non-jevons consumer of `Start(Provider: claude)` was
found on this host, but the search was by name and log, not by
import graph (fog-unknown 1).

## 7. What has no sidecar equivalent

Each of these is a capability the tmux path has and `ompAgentBackend`
(`omp_agent.go:119-121` claims `Session`, `Resume` only) does not:

| Capability | tmux Claude | Sidecar | Consumers today |
|---|---|---|---|
| Human attach (`AttachCommand`, `tmux attach -t @N`) | yes | none | `README.md:177`, `agents-guide.md:1365`; no jevons code reads `AttachCommand()` |
| Terminal bytes / `TermLogPath` | yes | none | grant wire carries `term_log_path`; no jevons reader found |
| `Rewind` | yes (`CapabilityRewind`) | refused by matrix | `daemon.TestRewindLive`, `TestRewindSessionLive`, `claudia broker` rewind op |
| Warm pool (`Acquire`) | yes | none | no jevons caller; `daemon.TestAcquire*` only |
| Crash survival across consumer death | tmux window outlives the consumer | sidecar outlives the daemon (`TestT865LiveSidecarSurvivesJevonsdBounce`) | equivalent exists |
| Steer / interrupt | ESC + queue | `OpSteer`, abort (`omp_agent.go:277-300`) | equivalent exists |
| Pane-title flight inference (jevons) | yes | needs a sidecar presence feed | `seat_observations.go` |
| **Live Session oracle for Claude** | the entire `CLAUDIA_LIVE` row (`TestGoalJourneyLiveBackends/claude`, MCP `/claude` subtests, readiness, multi-turn, crash, 🎯T110) all start `ProviderClaude` | **no `anthropic` subtest exists anywhere** | this is the largest blindspot: after the purge the Claude plan has no live Session journey at all until one is written |

## 8. Fog

**fog-known**

1. Dispatch is a single gap (`ompProviderID` lacks `claude`/`codex`);
   every legacy reach goes through it.
2. Grok and Cursor ACP Session code is already unreachable from `Start`
   and `Migrate`; only the registry adopt switch still names it.
3. The live fleet is 38/39 on the sidecar; the one tmux seat is this
   scout.
4. 14 `claude` + 2 `grok` `auto_start` grant rows are leftovers with no
   process behind them.
5. Pool has no consumer outside claudia's own tests.
6. The 🎯T1047 live specimen (CLI pane re-registered as `anthropic`
   after a restart) is reproduced by code reading: jevons
   `alignStartTransport` rewrites only fresh or explicit starts, while
   `RemintSubscription` rewrites every row at restart.

**fog-unknown**

1. Who produced the 7 `provider=codex` launches and the `arrai-*` /
   `pimp-*` `provider=claude` launches this week: a path jevons'
   rewrite does not cover (resume of an old row? `task_run`? another
   consumer?). Must be traced before the `codex` branch is deleted.
2. Whether the owner wants Task mode's `claude -p` / `codex exec` /
   `grok -p` / `agent --print` processes purged too (§9 Q1). Judge,
   Bedrock and Ollama Task backends are API paths and unaffected either
   way.
3. Whether human attach, terminal bytes and rewind may simply be lost
   (§9 Q2).
4. The Grok/Cursor live rows were not re-run in this scout; they are
   claimed sidecar-by-dispatch only.
5. What `tui_preview.go` still does for sidecar seats once the
   pane-frame path goes (the sidecar appends preview text; the
   Markdown conversion may be partly reusable).

**fog-blindspot**

1. No live oracle for an `anthropic` Session exists. Deleting the tmux
   row without first adding `anthropic` to `TestGoalJourneyLiveBackends`
   and the MCP live tests leaves the Claude plan unobserved; the gate
   table in `AGENTS.md` would be rewritten around nothing.
2. `internal/livegate` fails the hermetic gate when a live test exists
   without a table row; mass deletion of live tests must update the
   `AGENTS.md` tables and `live-gate-exclusions.json` in the same
   commit or `make gate` goes RED for every seat.

## 9. Blocking questions for the owner (per 🎯T1055's acceptance)

- **Q1 Scope.** Seat transport only (tmux Claude, Codex app-server, and
  the dead Grok/Cursor ACP Session code), or also Task mode's one-shot
  CLI processes? Task mode is how `migrationSummaryModel`-style helpers
  and `claudia task` run today; it would need a sidecar Task verb first.
- **Q2 Lost capabilities.** Accept losing human attach, terminal bytes
  and rewind outright, or require sidecar equivalents before removal?
  No jevons code reads any of the three; the docs advertise all three.
- **Q3 Leftover rows.** Remint the 14 `claude` / 2 `grok` `auto_start`
  grant rows onto the sidecar, or drop them?

## 10. Proposed order (for the implementer, not decided here)

0. **Close the gap first, delete second.** Land, in both repos in
   either order: jevons makes `SidecarLaunchProvider` unconditional at
   mint, resume and restart (🎯T1047 / 🎯T1055); claudia makes
   `ompProviderID` total (`claude` → `anthropic`, `codex` →
   `openai-codex`, or refuse those ids at `Start`/`Launch`/`Migrate`).
   Oracle: the daemon log shows zero `window=@` starts for a day.
1. Add an `anthropic` live Session journey (goal, MCP, send-and-wait)
   so the Claude plan keeps an oracle (fog-blindspot 1).
2. Delete the tmux path: §3.1–3.4, Pool, Rewind, the capability
   claims, the registry/daemon/broker-wire fields, the leftover grant
   rows (per Q3). Update `AGENTS.md` tables, `live-gate-exclusions.json`,
   CI's tmux install, README and agents-guide in the same commit.
3. Delete Codex app-server Session, Grok ACP/connect, Cursor ACP
   Session (keep the Task halves unless Q1 says otherwise); drop
   `ConnectURL`/`ConnectPID`/`GrokConnect`.
4. Collapse §5 to `ompProviderID` + `PlanProvider`.

Each step: `make gate`, then the live rows of every backend whose wire
moved. jevons' 🎯T1055 implementer should not delete `agenttmux`,
`panecensus`, `TmuxKillSwitch` or the upgrade handoff until claudia's
step 2 lands, and claudia should not land step 2 until jevons' step 0
has been observed for the fleet (otherwise a stray `claude` id becomes
a hard start failure instead of a tmux pane).

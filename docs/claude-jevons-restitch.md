# Claude ↔ Jevons re-stitch map (🎯T11.1)

**Status:** findings only (audit complete 2026-08-03). No product fixes in this slice.  
**Owner reframe:** Claude is not “broken.” Claudia fully supports `ProviderClaude`. Jevons ran Grok-only in production wiring; this map is the re-stitch status check after that period.

**Related baseline (claudia runtime, not Jevons stitch):**

- `docs/claude-provider-oracle-map.md` — ProviderClaude claim → hermetic oracle matrix
- SHA `eb20776` — RequireResume fail-closed for Claude + hermetic T11 oracles
- Hermetic package: `claude_provider_hermetic_test.go` (Start/Send/stream/tool_use/cancel/stop/resume/JSONL tail)

**Jevons selection surface (already achieved):** jevons 🎯T148 — verified against code below (not re-opened here).

**Oracle evidence for this audit:**

| Oracle | Result |
| --- | --- |
| `go test ./internal/cli ./internal/fleet ./internal/mcpserver -run 'Provider\|ResolveProvider\|SelectAgent\|ProviderFor\|RegisterProvider\|AgentStartProvider\|DefaultProvider'` (jevons) | PASS |
| `go test -run 'TestHermeticClaude\|TestSessionJSONLPath\|TestSessionExists\|TestTaskBackendForProvider\|TestHermeticTaskRunClaude' .` (claudia) | PASS |
| `go test ./internal/server -run 'ChatWire\|chatWire\|ClaudeShaped'` (jevons) | PASS |
| Production `~/.jevons/config.yaml` | no `provider:` field (falls through to Grok) |
| Production `~/.jevons/agents.json` | 14/14 agents `provider: grok`; `JEVONS_PROVIDER` unset |
| Live Claude Session E2E through current jevonsd | **not run** this slice — residual for 🎯T11.2 / live smoke |

---

## Surface status table

| Surface | Status | Evidence |
| --- | --- | --- |
| **Provider select** (`config.yaml`, `--provider`, `JEVONS_PROVIDER`, MCP `agent_start` / `thread_spawn` / `jwork` `provider=`) | **WORKS** | Precedence `override → config.yaml provider → JEVONS_PROVIDER → DefaultProvider(grok)`: `jevons/internal/cli/provider.go:27–47`. Flag: `cmd/jevonsd/main.go:56`, `123–130`. Config field: `internal/config/config.go:47–50`, `TestLoadProviderField`. MCP: `agent_start` `internal/mcpserver/agents.go:42`, `249–251`; `thread_spawn` `threads.go:69`, `214–220`; `jwork` `jwork.go:51`, `67–70`, `167`. Hermetic: `TestResolveProviderPrecedence`, `TestResolveProviderEnv`, `TestSelectAgentProviderViaServerDefault`. Pass-through (no allow-list) accepts `claude` / `bedrock`. |
| **Registry persists Provider; resume no-clobber to Grok** | **WORKS** | `claudia.AgentDef.Provider` JSON field: `registry.go:35–37`. Launch uses stored def: `registry.go:197–208`. Jevons selection: `SelectAgentProvider` keeps non-empty stored (`provider.go:50–67`); fleet backfill only when empty (`fleet.go:104–107`); agent_start applies Select then `Register` (`agents.go:249–268`). Hermetic: `TestSelectAgentProviderNoClobber`, `TestProviderForLaunchNoClobber`, `TestAgentStartProviderNotClobberedOnResumeLogic`, `TestRegisterProviderFromThread`. Grep: no unconditional `Provider = Grok` on Launch/resume. |
| **Launch path passes Provider into claudia.Config / AgentDef** | **WORKS** | `Registry.Launch` → `Start(Config{Provider: def.Provider, … RequireResume: def.Materialized})` (`registry.go:197–208`). Session fleet: `fleet.Launch` mints `AgentDef{Provider: prov}` (`fleet.go:89–99`) then `reg.Launch`. Task path: `jwork` `claudia.NewTask(TaskConfig{Provider: provider})` (`jwork.go:167+`). Claudia empty-provider default is still Claude (`agent.go` / `provider.go` `ProviderClaude`); Jevons never leaves new rows empty — always resolves via Select/Resolve (default Grok). |
| **Chat / event / ACP wire assumptions (Claude vs Grok)** | **WORKS** (UI wire) / **GAP** (product sides) | **WORKS:** `chatWireLine` normalises Grok ACP *and* Claude-shaped `Event.Raw` (`internal/server/chat_wire.go:23–109`, `isClaudeShaped`; tests in `chat_wire_test.go`). Overseer + workers use `SubscribeEvents` → shared `claudia.Event` (`chat.go:160–176`, `mcpserver/agents.go:wireAgentEvents`). Durable owner chat is **chatlog** (provider-agnostic), not ACP history. **GAP (jevons-po):** overseer MCP is **only** registered via `ensureGrokMCPServer` → `~/.grok/config.toml` (`cmd/jevonsd/main.go:496–508`, `892–908`). No Claude-equivalent MCP install; a Claude overseer would start **toolless** unless something else writes Claude MCP config. Comments still say “Grok-only” for scanner/sessions (`main.go:173`). `overseerUnavailableReason` hardcodes Grok CLI diagnosis (`main.go:919+`). Rewind always **rotates** session + chatlog recap (Grok ACP cannot truncate in place) — works for any provider but **does not** use Claude JSONL `Rewind` (`chat.go:334–376`). `agent_send` busy path is ACP “prompt already in flight” (`agent_send.go`) — Claude interrupt/queue may differ (smoke residual). |
| **Sessions dir / Materialized / RequireResume / transcript** | **WORKS** (claudia runtime) / **GAP** (Jevons inspect paths) | **WORKS (claudia):** `Materialized` → `RequireResume` on Launch (`registry.go:30–33`, `201`, `213–214`). Claude fail-closed when Materialized and JSONL missing (`agent.go:386–394`; `TestHermeticClaudeRequireResumeFailsClosed`, fix `eb20776`). Claude transcripts: `~/.claude/projects/<escaped-cwd>/<session>.jsonl` (`SessionJSONLPath`, `TestSessionJSONLPath` / `TestSessionExists` / `TestHermeticClaudeSessionJSONLTail`). **GAP (jevons-po):** default `SessionsDir` = `~/.grok/sessions` (`config.go:272`). `transcript.NewReader` + discovery scanner are Grok `chat_history.jsonl` layout (`internal/transcript/transcript.go` header + `findJSONL`; `main.go:173–196`). RHS fleet inspect / butler status / cost collector rooted there — **Claude Session JSONL is not discovered** by those paths. Transcript *parser* can decode some Claude nested envelopes when pointed at a file (`extractTurns` comments), but the **locator** is Grok-tree only. Production agents all Grok (see below). |
| **Default fleet still Grok?** | **WORKS** (yes — intentional) | Compile-time `DefaultProvider = claudia.ProviderGrok` (`provider.go:17–19`, `TestDefaultProviderIsGrok`). Boot: `ResolveProvider("", cfg.Provider)` with empty config/env → Grok (`main.go:129–130`). Production `~/.jevons/config.yaml` has **no** `provider:`; env unset; **14/14** registry agents `provider: "grok"`. Docs: README / agents-guide document empty → Grok. Selecting Claude is opt-in via flag/env/config/MCP `provider=claude`. |

---

## Jevons 🎯T148 claim vs code

| Claim (attestation on jevons T148) | Code verdict |
| --- | --- |
| No hardcode `cli.Provider=Grok` for all agents | **True** — default only when override/config/env/stored empty |
| `ResolveProvider`: override → config → `JEVONS_PROVIDER` → grok | **True** `provider.go:29–47` |
| `SelectAgentProvider` keeps stored on resume | **True** `provider.go:50–67` |
| `agent_start` / `thread_spawn` / `jwork` accept `provider` | **True** |
| `config.yaml` `provider` + `--provider` | **True** |
| Hermetic tests named in attestation | **PASS** (re-run this audit) |
| Residual: Claude full path is claudia T11 / not claimed by T148 | **Correct** — selection ≠ Session E2E product wire |

T148 is **achieved** as a selection surface. It does **not** claim Claude overseer tools, Grok-session transcript tree dual-write, or live Claude fleet smoke.

---

## What works end-to-end in theory (stitch spine)

When a caller sets `provider=claude` (or default is changed):

1. Selection resolves and is **persisted** on `AgentDef.Provider`.
2. Resume **does not clobber** back to Grok.
3. `Registry.Launch` passes Provider + Materialized→RequireResume into `claudia.Start`.
4. Claudia Session/Task for Claude remains hermetically proven (oracle map + `eb20776`).
5. Chat UI wire accepts Claude-shaped events (pass-through) as well as Grok ACP normalisation.

So the **control plane** (select → register → launch → event type) is re-stitched. Remaining risk is **product adapters** written for Grok production (MCP install, sessions tree, error copy, inspect).

---

## GAP ownership (for overseer / POs)

### → **jevons-po** (do not patch from claudia)

| ID | Gap | Why it matters for `provider=claude` |
| --- | --- | --- |
| J1 | Overseer MCP only via `ensureGrokMCPServer` / `~/.grok/config.toml` | Claude overseer starts without jevons MCP tools |
| J2 | `SessionsDir` + scanner + `transcript.Reader` Grok-tree only | Fleet inspect / thread status / some cost paths miss Claude JSONL |
| J3 | No hermetic **Jevons→claudia Session** stitch test with `provider=claude` | Selection tests only; no product integration oracle |
| J4 | Grok-centric boot diagnostics / comments (`overseerUnavailableReason`, “Grok-only” scanner) | Misleading ops when default flips or ad-hoc Claude is used |
| J5 | Rewind always rotates session (journal-first) | Acceptable behaviour; Claude-native JSONL rewind unused — document or branch later |
| J6 | `agent_send` busy/queue semantics tuned to ACP “prompt in flight” | Confirm Claude Interrupt/Send under concurrent notify (smoke) |

### → **claudia 🎯T11.2** (sibling smoke / residual)

| ID | Gap | Notes |
| --- | --- | --- |
| C1 | Live residual: `CLAUDIA_LIVE=1` Session readiness / multi-turn / pool | Declared on oracle map; not sole retirement evidence |
| C2 | Optional: hermetic “empty Provider in Registry behaves as Claude” already covered; keep green under stitch | No rewrite; smoke when selected from Jevons |
| C3 | No claudia bug found that blocks pass-through of Provider string from Jevons | Runtime spine WORKS |

### Out of scope here

- Ground-up Claude rewrite  
- T12 Bedrock  
- Editing jevons source from this agent  
- Large product fixes (findings only)

---

## Recommended next steps (not done in T11.1)

1. **T11.2 (claudia):** live or hermetic smoke that `provider=claude` Session Start/Send/stream still works on current claudia tip; cite SHA + test names.  
2. **jevons-po:** decide product policy — (a) dual-write Claude MCP config when provider=claude / overseer is Claude; (b) transcript/scanner abstraction by provider; (c) one integration test `agent_start(provider=claude)` with fake/hermetic backend if available.  
3. Keep **default fleet Grok** until (2) is ready; ad-hoc `provider=claude` for Task/`jwork` is the lowest-risk exercise path today (Task path avoids Grok sessions tree and overseer MCP).

---

## Attestation anchors

| Item | Ref |
| --- | --- |
| This document | `docs/claude-jevons-restitch.md` |
| Claudia oracle map | `docs/claude-provider-oracle-map.md` |
| RequireResume fix | `eb20776` |
| Jevons T148 | achieved; selection code under `internal/cli/provider.go`, fleet/MCP/server defaults |
| Audit agent | claudia-t11.1-restitch-audit under claudia-po |
| Ship plane | **not** opened — local commit only (T104) |

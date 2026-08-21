# Entropy audit — claudia — 2026-08-22

## Executive summary

- **Snapshot:** `/Users/marcelo/work/github.com/marcelocantos/claudia`, branch `master`, commit `715e175cee29a2e754d29eb912651709d73130f2` (`715e175 fix(mcp): one Authorize per 401 burst (jevons T531)`). Date 2026-08-22.
- **Initial dirty state** (`git status --porcelain=v1 -b`, recorded before any write): `## master...origin/master [ahead 11]`; untracked `?? .claudia-mcp-home/` and `?? scratchpad/` (user-owned; not touched). Working tree otherwise clean.
- **Scope:** Go module `github.com/marcelocantos/claudia` (library + `internal/` + `codex` + `grok` + `cmd/` + `specs/` + CI). Languages: Go (primary), Python oracles under `scripts/`, POSIX `scripts/tlc.sh`, TLA+ under `specs/`.
- **Exclusions:** `.claude/worktrees/`, `.vera/`, untracked `scratchpad/` and `.claudia-mcp-home/`, fixture JSONL under `testdata/` and `*/testdata/` (named when they are competing copies). Parent workspace `/Users/marcelo/work/github.com/marcelocantos/go.work` inflates `go list -m all` with sibling modules (jevons, …); `go.mod` itself lists a small direct set.
- **Headline mechanism:** a single public `claudia` package is a multi-provider runtime (Task + Session) with a published capability matrix and fail-closed field-fate audit — while a second public Codex Task package, a freeze-framed `STABILITY.md` snapshot four minors behind `Version`, and a half-wired lifecycle broker compete as sources of truth. At this HEAD the shipped hermetic path is red.
- **Highest-consequence findings:** ENT-001 (`MCPProxy` data race on concurrent 401), ENT-002 (T24 field-fate table missing `Config.GoalCompleteCheck`), ENT-003 (two public Codex Task implementations), ENT-004 (`STABILITY.md` snapshot stuck at v0.21.0 vs `Version` 0.25.0).
- **Unverified residue:** live provider gates (`CLAUDIA_*_LIVE`) not set; `make verify-mutation-evidence` and `make verify-specs` not re-run here; `staticcheck` cannot compile a go1.26 module (tool built with go1.25); no `govulncheck` / clone detector installed; Windows build not exercised.

## Scope and exclusions

In scope: package topology, public API, provider backends, broker/pool, MCP, Goal, CI/Makefile oracles, `STABILITY.md` / `agents-guide.md` / README vs code, hygiene declaration.

Named exclusions (not silent omissions):

| Tree | Role |
|---|---|
| `testdata/`, `codex/testdata/`, `internal/tmuxagent/testdata/`, `internal/broker/testdata/` | fixtures / pane captures |
| `scratchpad/` (untracked) | local repro specimens |
| `.claude/`, `.vera/` | gitignored tool caches / worktrees |
| `docs/papers/`, `docs/*-spike.md`, `docs/*-oracle-map.md` | design notes, not runtime |

## Commands run

| Command | Version / notes | Exit | Shipped vs auxiliary | Limitation |
|---|---|---|---|---|
| `git rev-parse HEAD`; `git status --porcelain=v1 -b` | git | 0 | provenance | — |
| `go version` | `go1.26.4 darwin/arm64` | 0 | toolchain | — |
| `go list -f '{{.ImportPath}}' ./...` | Go 1.26.4 | 0 | topology | 15 packages |
| `go list -m all` | Go 1.26.4 | 0 | auxiliary | **workspace-polluted** via `GOWORK=/Users/marcelo/work/github.com/marcelocantos/go.work`; includes jevons and many transitives not required by this `go.mod` |
| `go vet ./...` | Go 1.26.4 | 0 | shipped (`Makefile` `bullseye`) | does not see races |
| `go test ./... -run=^$ -count=1` | Go 1.26.4 | 0 | compile of test binaries | does not execute tests |
| `go test -race -count=1 ./...` | Go 1.26.4 | **1** | **shipped** (`Makefile` `bullseye`; `.github/workflows/test.yml` job `test`) | two failures in `github.com/marcelocantos/claudia`; other packages ok. Live tests skipped (gates unset). Duration ~30s for root package |
| `python3 scripts/check-stability-surface.py` | Python 3; `go doc` over tag worktree | **0** | shipped (`make verify-stability`; CI job `stability-surface`) | checks `STABILITY.md` against the **named** snapshot tag `v0.21.0`, not against HEAD or latest tag `v0.25.0` |
| `staticcheck -version` then `staticcheck …` | `staticcheck 2025.1.1 (0.6.1)` | compile error | auxiliary | built with go1.25.0; module requires go1.26.1 — no findings from this tool |
| `/Users/marcelo/.claude/skills/hygiene/hygiene_check.py` | uv-run | **1** | hygiene validator | `FileNotFoundError: hygiene.yaml` — posture not declared; not initialized |
| `shasum -a 256 testdata/codex/exec/* codex/testdata/exec/*` | — | 0 | fixture identity | four of six JSONL files byte-identical across trees |
| `wc -l` on `*.go` excluding `.claude/` / scratchpad | — | 0 | size locator | ~17577 prod / ~24916 test lines (test count includes nothing under excluded trees once filtered) |

Failed / skipped shipped checks:

- Hermetic `go test -race -count=1 ./...` **FAIL** — see ENT-001, ENT-002.
- Live gates (`CLAUDIA_LIVE`, `CLAUDIA_GROK_LIVE`, `CLAUDIA_CODEX_LIVE`, `CLAUDIA_BEDROCK_LIVE`, `CLAUDIA_OLLAMA_LIVE`) unset — skipped, not a pass (`AGENTS.md`).
- `make verify-mutation-evidence` and `make verify-specs` **not executed** this audit (CI jobs exist; residue).

## Dimension vector

| Dimension | State | Evidence summary | Change from baseline |
|---|---|---|---|
| Architecture topology | concern | One public SDK package with `taskBackend` / `agentBackend` dispatch; three Session transports (tmux, Grok ACP, Codex app-server); public `codex` package is a parallel Task API; broker socket is probed then ignored (T3). | n/a (first full audit) |
| Redundancy / sources of truth | concern | Dual Codex Task + parsers + auth + bin resolution + fixtures; `STABILITY.md` v0.21.0 vs `version.go` 0.25.0; public capability matrix vs narrower `providerCapabilities`. | n/a |
| Change amplification | concern | `agent.go` (1592 lines, 31 commits in window) and `Config` are the N-provider hub; T24 reflection audit exists and is currently red on a new field. | n/a |
| Local code quality | concern | MCP proxy race (ENT-001); large linear files that are still scannable; fail-closed refusals for unproven flags. | n/a |
| Correctness / verification | critical | Shipped hermetic suite with `-race` fails at HEAD (2 tests). Strong oracles exist (capability totality, T24 field fate, mutation-evidence, TLA+ mutants) but the standing `go test -race` job is red. Live backends remain owner-gated. | n/a |
| Security / dependencies | concern | Concurrent OAuth 401 path races (ENT-001); Codex subscription preflight is fail-closed (healthy); no secret-scan / `govulncheck` / Dependabot in-repo. `go.mod` direct deps are small (uuid, coder/websocket, AWS SDK for Bedrock). | n/a |
| Build / release / operations | concern | CI: test matrix ubuntu+macos, stability-surface, mutation-evidence, specs/TLC. `Version` 0.25.0 tagged; snapshot doc not refreshed. No Windows CI. No `hygiene.yaml`. | n/a |
| Documentation / governance | concern | `STABILITY.md` is a 1.0 contract inventory frozen four minors behind; T29 marked achieved; `agents-guide.md` Session table still says “Persistent PTY”. Consumer docs (`README`, `agents-guide`) otherwise track MCP/Goal/Ollama. | n/a |

Do not collapse this vector to a score.

## Observed architecture

### Entry points

- **Library (product):** `Start` / `Acquire` / `NewTask` / `Run` / `NewRegistry` in package `claudia`. `cmd/usage` is a thin CLI over `QueryAllPlanUsage`. Other `cmd/*` binaries are T28/T30 paste-submit reprobes, not the shipped API.
- **Session transports:** Claude → `internal/tmuxagent` (tmux control + paste); Grok → ACP stdio or connect-mode WebSocket (`grok_acp.go`, `grok_acp_connect.go`); Codex → `codex app-server` JSON-RPC (`codex_session.go`, `codex_app_server.go`). Bedrock and Ollama have Task only.
- **Task transports:** process JSONL (`claude`, `codex exec`, `grok -p`); HTTP (`bedrock` ConverseStream, `ollama` `/api/generate`).
- **Orthogonal package:** `claudia/grok` Realtime voice WebSocket client — documented as independent of `ProviderGrok`.
- **Public parallel package:** `claudia/codex` Task API that does **not** import or get imported by the root package.
- **Internal broker:** Unix-socket NDJSON lifecycle server (`internal/broker`) with TLA+ `specs/AgentLifecycle.tla`. Library `considerBroker` probes `TypeStatus` then always takes the direct backend (T3 not done).

### Directional dependencies (observed)

```
cmd/*            → claudia, internal/tmuxagent
claudia          → internal/tmuxagent, internal/broker, aws-sdk (Bedrock), coder/websocket (Grok connect)
claudia/codex    → (stdlib only)
claudia/grok     → coder/websocket
internal/broker  → (stdlib)
internal/tmuxagent → internal/testctlenv
```

No import cycle among these packages. Root does not import `codex` or `grok`.

### Declared vs observed rules

| Rule | Status |
|---|---|
| Provider gaps are published; silence is unsupported (`providerCapabilityClaims`, `TestProviderCapabilityMatrixIsTotal`) | declared and observed, enforced |
| Every `TaskConfig`/`Config` field honoured or refused (`TestProviderPathsHonourOrRefuseEveryRequestField`) | declared; **currently failing** (ENT-002) |
| `STABILITY.md` enumerates the snapshot tag’s `go doc` surface | declared and enforced **against v0.21.0**, not latest release |
| Live spawn/auth/turn-loop is owner-gated, not CI | declared (`AGENTS.md`, Makefile `live`) |
| Broker policy must not read wall clock / live sockets except at seams (`policy_guard_test.go`) | declared and observed |
| `CLAUDIA_NO_BROKER=1` skips all broker syscalls; otherwise probe-then-direct | declared; T3 spawn RPC not present |
| Windows unsupported for tmux Agent | declared in README; `*_windows.go` exists for flock/process |

### Inferred

- The intended 1.0 shape is “one `claudia` API, providers as backends, capability matrix as the contract.” The public `codex` package is an unresolved experiment from v0.21.0 (`STABILITY.md` Gaps).
- Broker is a future cross-process pool/AIMD daemon (T2.x identified); today’s pool is in-process `pool.go`.

## Findings

### ENT-001: MCPProxy concurrent-401 path races on `entry.probe`

- **Priority:** P0
- **Dimensions:** Correctness / verification; Security / dependencies; Local code quality
- **Status:** observed fact
- **Evidence:** `go test -race -count=1 ./...` exit 1; `--- FAIL: TestMCPProxyConcurrent401AuthorizesOnce`. Race: read `mcp_proxy.go:305` (`probe := entry.probe`) vs write `mcp_proxy.go:325` (`entry.probe = probe`) from `ServeHTTP` → `ensureAuth`. `authMu` is taken only later in `ensureOAuth` (`mcp_proxy.go:347-349`). Commit `715e175` is the T531 “one Authorize per 401 burst” fix; the test that asserts one browser tab is itself the race detector hit.
- **Mechanism:** N concurrent 401s share one `proxiedMCP`. Probe cache is mutated under `p.mu` but the nil-check read is unlocked. Two goroutines can both see `probe == nil`, both call `p.probe`, and race on the pointer. `authMu` does not cover this prefix, so the T531 serialization is incomplete.
- **Blast radius:** host-mounted `MCPProxy` (jevons `/upstream/…`) under parallel tool calls; token refresh/Authorize burst; any `-race` CI job on this commit.
- **Counterevidence checked:** `authMu` comment at `mcp_proxy.go:71-73` documents the intended lock; `ensureOAuth` does serialize Authorize. Single-threaded 401 tests can pass without `-race`. CI job `test` runs `-race` (`.github/workflows/test.yml:49`).
- **Smallest coherent remediation:** read/write `entry.probe` under `p.mu` (or take `authMu` at the start of `ensureAuth`); re-run `TestMCPProxyConcurrent401AuthorizesOnce` with `-race`.
- **Verification:** `go test -race -count=1 -run TestMCPProxyConcurrent401AuthorizesOnce .` must be green; a regression that unlocks the probe field must go red.
- **Ratchet candidate:** already the CI `-race` job — it is currently failing, not missing.

### ENT-002: T24 field-fate census red on `Config.GoalCompleteCheck`

- **Priority:** P1
- **Dimensions:** Correctness / verification; Change amplification
- **Status:** observed fact
- **Evidence:** same shipped `go test -race` run: `--- FAIL: TestProviderPathsHonourOrRefuseEveryRequestField` (`capability_audit_test.go:576-579`) with `claude Session GoalCompleteCheck: no disposition — silent drop` (and the same for Codex and Grok). Field added on `Config` at `agent.go:99-104` and copied onto the Agent at `agent.go:461`. `sessionFieldFates` lists `Goal` as `fateLocal` (`capability_audit_test.go:137`, `:158`, `:179`) but has no `GoalCompleteCheck` row. `SetGoalCompleteCheck` / `TestGoalCompleteCheckEndsLoopWithoutStatus` show the hook is real (`goal.go:49-58`, `goal_test.go:136`).
- **Mechanism:** T24 reflects over exported `Config` fields and requires a disposition per Session provider. Adding a host-local func field without updating the table trips the oracle — which is the oracle working. HEAD still ships with the suite red.
- **Blast radius:** `make bullseye` / CI `test` job; anyone adding the next `Config` field cannot tell whether a new silent-drop is real or this leftover.
- **Counterevidence checked:** the field is host-owned (not sent to a provider); behaviour is tested in `goal_test.go`. This is not a silent drop of a provider flag. The failure is census lag, not a Goal-loop logic bug.
- **Smallest coherent remediation:** add `GoalCompleteCheck: {fateLocal, "host completeness hook; never sent to the provider"}` for Claude/Grok/Codex Session maps (Bedrock/Ollama Session already fail closed as a whole).
- **Verification:** `go test -count=1 -run TestProviderPathsHonourOrRefuseEveryRequestField .` green; deleting that row must fail the census.
- **Ratchet candidate:** already T24; keep it. Do not weaken the test to ignore func fields.

### ENT-003: Two public Codex Task implementations that cannot share a bugfix

- **Priority:** P1
- **Dimensions:** Redundancy / sources of truth; Architecture topology; Change amplification
- **Status:** observed fact (named in `STABILITY.md`; still true at HEAD)
- **Evidence:**
  - Root does not import `claudia/codex`; `codex` does not import root (`go list` imports).
  - Root spawn: `task.go:651-671` (`codexTaskBackend.RunTask` → `ensureCodexSubscriptionAuth` + `resolveCodexBin`).
  - Subpackage spawn: `codex/task.go:154-172` (`ensureSubscriptionAuth` + `resolveBin`).
  - Parsers: `codexTaskParser.Parse` `task.go:945-988` vs `codex/parse.go:11-56` (same `thread.started` / `turn.completed` mapping; root parser also reads `ReasoningOutputTokens` and ignores it).
  - Bin candidates duplicated: `provider.go:78-88` vs `codex/resolve.go:85-93`.
  - Auth duplicated: `PreflightCodexAuth` (`codex_auth.go:81`) vs `ensureSubscriptionAuth` (`codex/resolve.go:107`) — different APIs (structured preflight vs error), same `auth.json` / `OPENAI_API_KEY` rules.
  - Fixtures: `testdata/codex/exec/{error,failure,malformed,success}.jsonl` SHA-256-identical to `codex/testdata/exec/` of the same names; `auth_fail.jsonl` and `rate_limit.jsonl` exist only in the subpackage tree.
  - `STABILITY.md:474-482` already calls this a 1.0 liability; `codex.Usage` still lacks `CacheCreationInputTokens` (`STABILITY.md:204`).
  - `codex/doc.go:7-10` advertises a public Task surface “mirroring” claudia Task mode.
- **Mechanism:** a Codex exec JSONL/auth/argv bug must be fixed twice or the two APIs drift. They have already drifted (usage fields, extra fixtures, reasoning tokens). Callers can pick either entry point.
- **Blast radius:** any Codex Task consumer; 1.0 API freeze (T1); parser/auth/bin changes.
- **Counterevidence checked:** Session Codex (app-server) lives only in the root package — the duplication is Task-shaped, not Session. Subpackage is marked Fluid. Hermetic tests exist on both sides (`task_spawn_test.go`, `codex/task_test.go`).
- **Smallest coherent remediation:** make `claudia/codex` `internal` (or unexport it) and have `codexTaskBackend` call that implementation; or delete the root Codex Task spawn and wrap the subpackage. Do not keep two public `NewTask`s. Align `Usage` fields in the same change.
- **Verification:** a test that `claudia.NewTask({Provider: ProviderCodex})` and `codex.NewTask` share one parser/auth/bin (import graph: root imports `codex` or vice versa, not neither); mutating `codex exec` JSONL mapping in one place must fail both suites.
- **Ratchet candidate:** `go list` / archtest: package `claudia` must import `claudia/codex` for Codex Task, **or** `claudia/codex` must not be a public package (move under `internal/`). `STABILITY.md` Gaps item struck when done.

### ENT-004: `STABILITY.md` snapshot is four minors behind the released module

- **Priority:** P1
- **Dimensions:** Documentation / governance; Redundancy / sources of truth
- **Status:** observed fact
- **Evidence:**
  - Snapshot line `STABILITY.md:15`: `v0.21.0 (tagged 2026-08-10)`.
  - `version.go:9`: `const Version = "0.25.0"`. Tags `v0.22.0`…`v0.25.0` exist; `v0.25.0` is `619f459` (2026-08-19, MCP).
  - `STABILITY.md:17-19` says `ProviderOllama` is “Present at HEAD, not yet released.” `git grep ProviderOllama v0.25.0` hits `ollama.go`, `capability.go`, `provider.go`.
  - Snapshot capability table `STABILITY.md:314-326` has no Ollama column, no `CapabilitySandboxPolicy` / `CapabilityExtraArgs`, and lists Codex Session as **Experimental**. HEAD claims Codex Session **Supported** (`capability.go:217-219`).
  - Snapshot `Config` row `STABILITY.md:56` omits `Goal`, `GoalCompleteCheck`, `MCPServers`, `MCPExclusive`, `SandboxMode`.
  - `make verify-stability` **PASS** here: 192 items, 174 fields, 17 env vars, 0 mismatches — against tag `v0.21.0`.
  - T29 (`bullseye.yaml:1831-1836`) is **achieved** with attestation that the snapshot stays at v0.21.0 “until this release’s tag exists; v0.22.0 surface … is snapshotted after the tag.” Those tags now exist; the snapshot was not refreshed.
  - T29 acceptance bullet 1: “names the **latest released tag** (>= v0.21.0)”. Latest is v0.25.0, not v0.21.0.
- **Mechanism:** the stability oracle is “document matches the tag it names,” not “named tag is HEAD/latest.” CI stays green while the 1.0 shakeout inventory (T1 depends on `STABILITY.md` Gaps) omits MCP, Goal, Ollama, `MCPProxy`, `AuthorizeMCP`, extra capabilities. The “not yet released” Ollama sentence is false.
- **Blast radius:** 1.0 cut (T1); consumers reading `STABILITY.md` as current contract; capability docs vs `CheckCapability`.
- **Counterevidence checked:** the snapshot design is deliberate (`STABILITY.md:15-19`, `scripts/check-stability-surface.py:1-7`). README / `agents-guide.md` do document MCP, Goal, Ollama. The freeze is not total documentation failure — it is the 1.0 inventory that drifted.
- **Smallest coherent remediation:** retarget `Snapshot as of:` to `v0.25.0` (or current tag), re-derive tables with `make verify-stability`, add MCP/Goal/Ollama/capability rows, delete the false Ollama “not yet released” note, and only then keep T29 achieved.
- **Verification:** `make verify-stability` against a snapshot line that equals `git describe --tags --abbrev=0` (or `Version`); a new exported identifier at HEAD without a table row fails.
- **Ratchet candidate:** extend `check-stability-surface.py` (or a sibling) to fail when the snapshot tag ≠ latest `v*` tag on the repo, unless an explicit “HEAD lag” waiver file exists. Do not ratchet that without owner consent — it changes T29’s meaning.

### ENT-005: Root package is the N-provider change hub

- **Priority:** P2
- **Dimensions:** Change amplification; Architecture topology
- **Status:** observed fact
- **Evidence:** `go list` — package `claudia` has 31 prod files / 47 test files. `agent.go` 1592 lines (Start, Config, Agent, backend switch `agent.go:300-312`, Claude tmux ops, JSONL tail). `task.go` 1238 lines (Task types, backend switch `task.go:257-271`, Claude/Codex/Grok spawn + parsers). `mcp.go` 846, `registry.go` 664, `capability.go` 508. Git churn since 2026-02-01: `agent.go` 31, `registry.go` 17, `task.go` 11, `grok_acp.go` 12. Adding a `Config` field requires T24 rows for every Session provider (ENT-002 is the worked instance).
- **Mechanism:** one shared `Config`/`TaskConfig` plus per-provider backends in the same package means a Claude-tmux paste fix, a Grok ACP permission option, and a Codex app-server field all land in the same compilation unit and often the same review surface. The backend interfaces (`taskBackend`, `agentBackend`) already isolate spawn; the files have not followed the interfaces out of the root package.
- **Blast radius:** every provider feature; review load; merge conflicts on `agent.go`.
- **Counterevidence checked:** this is the intended SDK shape (one import path). Capability matrix + T24 exist specifically to make hub edits fail closed. `internal/tmuxagent` already extracted the PTY/paste problem. Extracting backends prematurely could hide provider-specific refusals.
- **Smallest coherent remediation:** move Claude/Grok/Codex/Bedrock/Ollama **implementations** under `internal/` (keep types and `Start`/`NewTask` in root). Do not split the public API. Do this after ENT-003 so Codex has one home.
- **Verification:** import-graph test: `claudia` prod files other than `task.go`/`agent.go`/`provider.go`/`capability.go` do not reference `exec.Command` for provider binaries.
- **Ratchet candidate:** package-boundary test once the move exists — not before.

### ENT-006: Lifecycle broker is a second authority that Start still ignores

- **Priority:** P2
- **Dimensions:** Architecture topology; Change amplification
- **Status:** observed fact (T3 identified)
- **Evidence:** `broker_mode.go:12-17` states the RPC client that turns a listening broker into a spawn is T3; until then a listening broker is probed with status and the fallback starts the agent. `considerBroker` (`broker_mode.go:29-42`) dials, writes `TypeStatus`, reads, discards. `Start` (`agent.go:387`) → `startConsideringBroker` (`broker_mode.go:44-48`) always `startWithBackend`. `Task.Run` (`task.go:391-393`) same probe. Timeout `brokerProbeTimeout = 2s` (`broker_mode.go:19`). `internal/broker/server.go:14-18` spawn allocates a session record, not a provider process. T3 `bullseye.yaml:1849-1859` still `identified`. In-process pool remains `pool.go` (T2.3 identified: cross-consumer pool).
- **Mechanism:** two spawn/lifecycle designs (direct tmux/ACP/app-server vs broker socket) must be kept compatible. Every `Start`/`Task.Run` may pay a 2s dial deadline if a stale `broker.sock` exists (`considerBroker` sets that deadline on a successful Dial). Policy oracles (Clock, TLA+) describe a daemon that the library does not yet use for spawn.
- **Blast radius:** all Session/Task starts when a socket is present; T2.x work will fork `pool.go` vs broker pool.
- **Counterevidence checked:** `CLAUDIA_NO_BROKER=1` is a real escape hatch (`internal/broker/socket.go:68-75`); tests assert probe-then-direct (`broker_mode_test.go`). Broker is `internal` so it does not expand the 1.0 public surface (`internal/broker/clock.go:16-18`). Incomplete is explicit, not accidental.
- **Smallest coherent remediation:** keep T3 as the pivot; until then consider failing fast on a listening broker rather than probing and ignoring, or skip the probe unless a feature flag opts in — so a stale socket cannot stall Start. Do not implement AIMD/reap in two places.
- **Verification:** test that a listening broker that never answers does not delay `Start` beyond a bound, **or** that `Start` uses spawn RPC (T3 acceptance).
- **Ratchet candidate:** T3 acceptance tests; optional: Start latency test with a black-hole socket.

### ENT-007: Internal capability wiring is a narrower second matrix

- **Priority:** P2
- **Dimensions:** Redundancy / sources of truth; Correctness / verification
- **Status:** observed fact
- **Evidence:** public claims `providerCapabilityClaims` (`capability.go:139-141`) cover `reportedCapabilities()` including `CapabilityToolRestrictions`, `CapabilitySandboxPolicy`, `CapabilityExtraArgs` (`capability.go:121-136`). Internal `providerCapabilities` (`capability.go:466-480`) has no fields for those. `backendCapabilityNames` therefore cannot ratchet them. `capability.go:466-470` cites `TestProviderCapabilityMatrixMatchesBackendClaims` — **that test name does not exist**. Per-provider tests do: `TestCodexCapabilityMatrixMatchesBackendClaims`, `TestGrokCapabilityMatrixMatchesBackendClaims`, `TestOllamaCapabilityMatrixMatchesTheBackend`. No `TestBedrockCapability*` match test. `grok_capability_test.go:88-101` documents the tool_restrictions blind spot and adds a separate argv-builder test.
- **Mechanism:** flipping a public claim to `supported` without wiring argv/API is the exact fake-parity the matrix exists to prevent. For tool_restrictions the Grok extra test closes it; Bedrock/Ollama extra capabilities rely on prechecks + the public claim table. The comment’s missing unified test is a stale pointer.
- **Blast radius:** next capability (image input, web search binding, Grok `--deny` translation).
- **Counterevidence checked:** `TestProviderCapabilityMatrixIsTotal` still requires every provider to claim every reported name. Task prechecks refuse `DisallowTools` on non-Claude. T24 covers request fields.
- **Smallest coherent remediation:** rename/replace the cited test with one loop over all providers; extend `providerCapabilities` **or** keep it narrow and list the uncovered capabilities in one table test (Grok’s argv pattern generalized). Add Bedrock to the match loop.
- **Verification:** a test that fails if `ProviderCapabilityStatus==supported` and no backend `Capabilities()` bit **or** no named precheck/argv assertion exists for that capability.
- **Ratchet candidate:** that unified test in `provider_test.go`.

### ENT-008: `agents-guide.md` Session model is still Claude-PTY

- **Priority:** P2
- **Dimensions:** Documentation / governance
- **Status:** observed fact
- **Evidence:** `agents-guide.md:22-26` table: Session process model “Persistent PTY”, output “JSONL transcript + raw PTY”. Grok Session is ACP stdio/WebSocket (`README.md:235-237`); Codex Session is app-server JSON-RPC (`README.md:239-241`). Later sections of the same guide describe Goal and LoadMCP correctly (`agents-guide.md:123+`, `:136+`). Package comment `agent.go:14-17` still introduces Session as tmux + JSONL as if that were the only transport.
- **Mechanism:** an integrating agent that stops at the mode-choice table will wire PTY/tmux assumptions onto Grok/Codex (attach, term log, paste).
- **Blast radius:** LLM consumers of `agents-guide.md` (the file README tells agents to load).
- **Counterevidence checked:** README Session examples are provider-accurate; capability matrix publishes tmux/term-log unsupported for Grok/Codex.
- **Smallest coherent remediation:** change the table to “persistent process (tmux PTY for Claude; ACP/app-server stdio otherwise)” and “events via JSONL tail or in-process RPC.” Update the package comment to match `Start`’s own doc (`agent.go:374-376`).
- **Verification:** a doc grep is the wrong ratchet; prefer a short `TestPackageDocMentionsNonTmuxSessions` on the `Start` comment, or include the table in a human release checklist.
- **Ratchet candidate:** optional string test on `agent.go` package comment; do not grep `agents-guide.md` as a journey.

### ENT-009: `grok.DialArgs` still leaks the WebSocket concrete type

- **Priority:** P3
- **Dimensions:** Architecture topology; Documentation / governance
- **Status:** observed fact
- **Evidence:** `STABILITY.md:483-486` (names `nhooyr.io/websocket`); actual type `grok/realtime.go:110-116` uses `github.com/coder/websocket` (`go.mod:6`). Import path changed; the leak remains.
- **Mechanism:** swapping the WS library is a breaking API change for a test seam.
- **Blast radius:** `claudia/grok` Realtime consumers only (orthogonal to `ProviderGrok`).
- **Counterevidence checked:** package is documented as standalone (`README.md:304-311`); Dial is a test seam; Fluid.
- **Smallest coherent remediation:** wrap Dial behind an interface that returns `io.ReadWriteCloser` (or unexport `DialArgs` and inject via `internal` in tests).
- **Verification:** `go doc` of `claudia/grok.DialArgs` has no `websocket.` identifiers.
- **Ratchet candidate:** `check-stability-surface.py` already lists the field; 1.0 Gaps item.

### ENT-010: `TaskConfig.ClaudeID` is the resume handle for every provider

- **Priority:** P3
- **Dimensions:** Local code quality; Documentation / governance
- **Status:** observed fact
- **Evidence:** field docs `task.go:188-191` say “claude session ID”; Codex/Grok hermetic tests assert `task.ClaudeID()` after spawn (`task_spawn_test.go:189-190`, `:237-238`). `agents-guide.md:48-49` already warns the name is reused.
- **Mechanism:** a caller skipping the guide will not set `ClaudeID` for Codex resume, or will assume Claude JSONL layout.
- **Blast radius:** Task resume across providers.
- **Counterevidence checked:** renaming is a 1.0 breaking change; documented. Pre-1.0 is the window to rename to `SessionID` (Codex subpackage already uses `SessionID`).
- **Smallest coherent remediation:** alias `SessionID` on `TaskConfig` and deprecate `ClaudeID` before 1.0, or rename in the T1 breaking cut.
- **Verification:** `go doc TaskConfig` leads with a provider-neutral name.
- **Ratchet candidate:** STABILITY Gaps item (alongside Purpose typing).

### ENT-011: No vulnerability, secret, or dependency-update gate in CI

- **Priority:** P3
- **Dimensions:** Security / dependencies; Build / release / operations
- **Status:** observed fact
- **Evidence:** `.github/workflows/` contains only `test.yml` and `specs.yml`. No Dependabot/Renovate file, no `govulncheck`, no secret scan. `staticcheck` is installed locally but not CI and cannot build this module (go1.25 vs go1.26).
- **Mechanism:** AWS SDK / websocket / Go stdlib CVEs and accidental credential files can land without a standing check. Codex/Grok/Claude auth files are read from the user home; tests isolate paths, production does not scan.
- **Blast radius:** supply chain of a library that shells out to CLIs and proxies MCP HTTP.
- **Counterevidence checked:** `go.mod` direct require set is small; live tests are opt-in; MCP proxy does not persist tokens (`mcp_proxy.go:48-49`). Apache-2.0 `LICENSE` present. This is an absence, not a known vuln.
- **Smallest coherent remediation:** add a CI `govulncheck ./...` job when a go1.26-capable build exists; optional secret scan on PRs. Do not add Dependabot noise without owner preference.
- **Verification:** CI job exists and fails on a known vulnerable pin (or a synthetic).
- **Ratchet candidate:** hygiene item `security.vuln-scan` **if** hygiene is onboarded later. Do not initialize `hygiene.yaml` in this audit.

## Redundancy and competing-source-of-truth inventory

| Concept | Authorities | Drift already? | Disposition |
|---|---|---|---|
| Codex Task spawn/parse/auth/bin | `claudia` `codexTaskBackend` vs public `claudia/codex` | yes (Usage fields, fixtures, reasoning tokens) | ENT-003 |
| Codex exec JSONL fixtures | `testdata/codex/exec` vs `codex/testdata/exec` | extra files only in subpackage; four files identical | fold into ENT-003 |
| Public capability matrix | `providerCapabilityClaims` vs `STABILITY.md` snapshot table vs internal `providerCapabilities` | yes (Codex Session Experimental vs Supported; missing Ollama/sandbox/extra_args in snapshot) | ENT-004, ENT-007 |
| Public API inventory | `STABILITY.md` @ v0.21.0 vs `version.go` / tags / `go doc` at HEAD | yes | ENT-004 |
| MCP attach | `Config.MCPConfig` path vs `Config.MCPServers` vs `EnsureMCP` writing Claude JSON + Grok TOML + Codex TOML | overlay is intentional (`mcp.go:247-277`) | retain; T24 tracks both |
| Session process model | tmux PTY vs ACP vs app-server vs docs table “Persistent PTY” | docs lag | ENT-008 |
| Agent pool | in-process `pool.go` vs broker warm pool (T2.3) | second not built | ENT-006 |
| Goal completeness | `GOAL_STATUS` lines vs `GoalCompleteCheck` vs T24 table | census lag | ENT-002 |
| WebSocket library name | STABILITY `nhooyr.io/websocket` vs `github.com/coder/websocket` | naming only | ENT-009 |

Deliberate duplication retained: Claude Task JSONL parser (`ParseTaskLine`) vs Session `parseEvent` (different wire formats); Grok Realtime package vs `ProviderGrok` (different products).

## Healthy structure and deliberate exceptions

- **Capability totality:** `TestProviderCapabilityMatrixIsTotal` (`provider_test.go:267`) fails if a provider is silent. Production `Start` gates on `CheckCapability(..., CapabilitySession)` (`agent.go:377-386`).
- **T24 field-fate census:** reflection over `TaskConfig`/`Config`; it caught ENT-002. Mutation tests exist (`TestDeclaringAConsumedFieldUnsupportedGoesRed`).
- **Fail-closed tool restrictions:** Grok/Codex/Bedrock/Ollama refuse `DisallowTools` rather than spawn fully armed (`task.go:747-777` and siblings). Incident note on `BaseDisallowedTools` (`task.go:121-131`).
- **Mutation-evidence oracle:** `scripts/check-mutation-evidence.py` + `--prove-teeth`, CI job `mutation-evidence` (`Makefile:38-41`, `.github/workflows/test.yml:68-93`). Not re-run this audit.
- **TLA+ broker lifecycle with mutants:** `make verify-specs` / `.github/workflows/specs.yml`; correct config green, three fault configs must fail (`Makefile:60-72`, `specs/AgentLifecycle.cfg:1-16`).
- **tmuxagent isolation:** paste/submit/readiness live in `internal/tmuxagent` with frame fixtures; policy comments cite T28/T30/T305.
- **Broker policy guard:** `//claudia:policy` + `pinnedPolicyFiles` includes `pool.go` (`internal/broker/policy_guard_test.go:87-105`).
- **Grok Realtime isolation:** separate package, no import from root.
- **Live-test doctrine:** `AGENTS.md` and `Makefile` `live` name the tests; CI never sets gates. Honest skip, not a fake green.
- **Windows:** README `README.md:16` unsupported; `flock_windows.go` is a build-tagged helper, not a claimed port.
- **cmd reprobers:** `cmd/t28repro` et al. are labelled T28/T30 live oracles, not a second product.

## Hygiene posture

**Hygiene posture not declared.** `hygiene.yaml` is absent. Validator:

```
/Users/marcelo/.claude/skills/hygiene/hygiene_check.py
FileNotFoundError: .../claudia/hygiene.yaml
exit 1
```

Not initialized (campaign constraint).

Observed CI/Makefile reality (for a future onboard, not a ratchet this run):

- correctness: `go test -race -count=1 ./...` (currently red), `go vet`, mutation-evidence, TLA+ specs, live Makefile target (opt-in)
- docs: `make verify-stability` (snapshot-scoped)
- quality: no format/lint job in CI (`staticcheck` not wired)
- security: no scanner job
- release: `Version` constant; `/release` skill (not in-repo workflow)
- governance: Apache-2.0 `LICENSE`, `STABILITY.md`, bullseye targets

Entropy findings suitable as later hygiene items: ENT-001/002 as `correctness.hermetic-race` (must be green); ENT-004 as `docs.stability-snapshot-is-latest-tag`; ENT-011 as `security.vuln-scan` (planned, above floor).

## Oracle coverage and residue

| Property | Decided by |
|---|---|
| Hermetic unit/parse/spawn-fake/Goal/MCP proxy | shipped `go test -race ./...` — **currently FAIL** (ENT-001, ENT-002) |
| Public surface of a named release tag | `make verify-stability` — PASS vs v0.21.0 |
| Quoted mutation evidence still bites | CI `mutation-evidence` — **not re-run here** |
| Broker lifecycle safety (no double-own, no reap-while-held, no send-after-reap) | TLC + mutants — **not re-run here** |
| Provider capability totality | `TestProviderCapabilityMatrixIsTotal` (hermetic; part of failing package run — this test was not among the two FAILs) |
| Request-field honour/refuse | T24 — FAIL (ENT-002) |
| Live spawn/auth/turn/Goal/MCP on real CLIs | `make live` + env gates — **skipped** (unset) |
| Windows Agent | accepted unsupported |
| Dependency CVEs | nothing |
| 1.0 API completeness vs HEAD | `STABILITY.md` — stale (ENT-004) |

**Owner residue (intent, not mechanical leftover):**

1. Should `claudia/codex` be public after 1.0, or an internal backend? (ENT-003; already posed in STABILITY Gaps.)
2. When should the stability snapshot track latest tag vs a frozen shakeout baseline? (ENT-004 vs T29 attestation.)
3. Is the broker probe-on-every-Start acceptable until T3, or should it be opt-in? (ENT-006.)
4. Rename `TaskConfig.ClaudeID` before 1.0? (ENT-010.)

## Remediation sequence

1. **Repair the shipped hermetic oracle (ENT-001, ENT-002).** Fix the `MCPProxy` probe race; add the T24 `GoalCompleteCheck` disposition. `go test -race -count=1 ./...` must be green. This unblocks every other claim about HEAD.
2. **Refresh `STABILITY.md` to the latest tag (ENT-004)** so 1.0 Gaps describe the API that actually shipped (MCP, Goal, Ollama, capability extras). Keep `verify-stability` as the enumerator.
3. **Converge Codex Task (ENT-003)** onto one implementation; delete or unexport the other; share fixtures. Do this before any further Codex exec parser work.
4. **Docs table/package comment (ENT-008)** once Session transports are described as they are.
5. **Capability wiring test (ENT-007)** as a small ratchet on the existing matrix.
6. **Broker (ENT-006)** only as T3 — do not grow a second pool. Consider skipping the status probe until spawn RPC exists.
7. **Optional 1.0 polish:** DialArgs (ENT-009), ClaudeID rename (ENT-010), `govulncheck` (ENT-011), extract backends (ENT-005) after Codex is single-homed.
8. Re-run this audit on the same definitions. Do not initialize `hygiene.yaml` unless asked; if onboarded, set floors to held reality (correctness currently cannot claim a green hermetic floor until step 1).

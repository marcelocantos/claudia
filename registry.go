// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/google/uuid"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// AgentDef is the persistent definition of a named agent stored in a [Registry].
type AgentDef struct {
	// Name is the unique identifier for this agent within its registry.
	Name string `json:"name"`

	// WorkDir is the working directory the agent process runs in.
	WorkDir string `json:"workdir"`

	// SessionID is the provider session ID used for resume/load. It is
	// assigned automatically when the agent is first registered. Grok
	// ACP may replace it with the id returned by session/new.
	SessionID string `json:"session_id"`

	// Materialized records that SessionID has hosted a real conversation
	// (durable transcript evidence such as a Claude session JSONL, not
	// merely a successful process Start). Once set, launches pass
	// RequireResume so a failed session load can never silently mint a
	// replacement id. Never flip this on bare Start alone — use
	// [Registry.MarkMaterialized] after evidence appears, or rely on
	// Launch promoting from [SessionExists] when JSONL is already present.
	//
	// Grok, Codex and Cursor also RequireResume on a persisted SessionID that
	// was not minted in this process (bounce / reload), even when
	// Materialized is still false — those providers' first mint leaves
	// Materialized unset, and treating that as "never-materialized" is
	// how a SIGHUP reminted a live Codex thread (jevons 🎯T545.1).
	Materialized bool `json:"materialized,omitempty"`

	// Provider selects the runtime (claude, codex, grok, cursor). Empty means
	// ProviderClaude. Grok Session uses ACP over `grok agent stdio`.
	// Cursor Session uses ACP over `agent acp`.
	Provider Provider `json:"provider,omitempty"`

	// Model overrides the default model (e.g. "opus", "sonnet", "grok-4").
	Model string `json:"model,omitempty"`

	// AutoStart causes this agent to be launched by [Registry.StartAll].
	AutoStart bool `json:"auto_start"`

	// Parent is the name of the agent that spawned this one (fleet
	// lineage). Empty means root / unknown (typically the overseer).
	// Used for kill authorization: only ancestors may kill descendants.
	Parent string `json:"parent,omitempty"`

	// Purpose classifies the agent in a unified fleet model (work | aside |
	// overseer). Empty means work. Aside agents are side-chat participants
	// that share the same registry and deliver path as workers; UI may
	// present them differently without a second store.
	Purpose string `json:"purpose,omitempty"`

	// Role is the declarative fleet role the agent was spawned as
	// (e.g. worker, auditor, product-owner). Empty means derive from
	// Purpose / name heuristics. Distinct from Purpose: purpose is the
	// sandbox/goal class; role carries per-type doctrine (jevons 🎯T511 /
	// 🎯T536.2).
	Role string `json:"role,omitempty"`

	// Description is an optional owner-facing label (e.g. aside title for
	// purpose=aside rows). Empty means UI falls back to Name.
	Description string `json:"description,omitempty"`

	// TargetID is an optional bullseye target id this agent is engaged on
	// (e.g. "T10.2"). Jevons frontier UI merges fleet rows with the frontier
	// by exact TargetID equality — never by parsing agent names (🎯T198).
	// Empty means not engaged on a specific ledger target.
	TargetID string `json:"target_id,omitempty"`

	// DisallowTools lists additional tool names to disallow beyond the
	// claudia defaults (Claude Session). Grok ACP may ignore these.
	DisallowTools []string `json:"disallow_tools,omitempty"`

	// ConnectURL / ConnectPID persist Grok connect-mode serve endpoint so
	// a restarted consumer can reattach to the same process (jevons 🎯T40).
	// Empty / 0 means stdio mode or never launched in connect-mode.
	ConnectURL string `json:"connect_url,omitempty"`
	ConnectPID int    `json:"connect_pid,omitempty"`

	// GrokConnect forces connect-mode on next Launch for ProviderGrok.
	// Also enabled via CLAUDIA_GROK_CONNECT env.
	GrokConnect bool `json:"grok_connect,omitempty"`

	// SandboxMode is the Codex app-server sandbox for ProviderCodex
	// Session (e.g. "workspace-write"). Empty keeps claudia's safe
	// default of read-only (🎯T37).
	SandboxMode string `json:"sandbox_mode,omitempty"`

	// SandboxWritableRoots and SandboxNetworkAccess widen that sandbox
	// beyond the working directory (🎯T598). They reach Codex through
	// CODEX_HOME/config.toml, not through thread/start, which cannot
	// express them. Persisted so a relaunch keeps the access the seat's
	// mission needs — a seat silently narrowed on restart fails at its
	// next gate, not at launch.
	SandboxWritableRoots []string `json:"sandbox_writable_roots,omitempty"`
	SandboxNetworkAccess bool     `json:"sandbox_network_access,omitempty"`

	// Goal is the durable host-owned Session objective (🎯T39). Copied
	// onto Config.Goal at Launch/Adopt so a provider switch keeps the
	// same objective. Empty means one-shot Send.
	Goal string `json:"goal,omitempty"`

	// MCPServers is the session-scoped MCP list (🎯T40). Copied onto
	// Config.MCPServers at Launch. Session never writes provider config
	// files.
	MCPServers []MCPServer `json:"mcp_servers,omitempty"`

	MCPExclusive bool `json:"mcp_exclusive,omitempty"`

	// PermissionMode, MCPConfig, ExtraArgs and TermLogPath are the
	// Session-only Config fields a definition can carry so a daemon grant
	// (🎯T2.10) is the whole Config, not most of it. Empty keeps each
	// Start default (bypassPermissions; no --mcp-config; no extra argv;
	// the XDG terminal log path).
	PermissionMode string   `json:"permission_mode,omitempty"`
	MCPConfig      string   `json:"mcp_config,omitempty"`
	ExtraArgs      []string `json:"extra_args,omitempty"`
	TermLogPath    string   `json:"term_log_path,omitempty"`
}

// Canonical Purpose values for [AgentDef.Purpose].
const (
	PurposeWork     = "work"
	PurposeAside    = "aside"
	PurposeOverseer = "overseer"
)

// Registry manages named [Agent] processes keyed by [AgentDef]. Definitions
// are persisted to a JSON file at the path passed to [NewRegistry], so agents
// survive process restarts. Typical usage: call [Registry.Register] (or
// [Registry.EnsureAgent]) once at startup, then [Registry.Launch] to get a
// live [Agent], and [Registry.StopAll] on shutdown.
type Registry struct {
	path string

	mu     sync.Mutex
	agents map[string]*AgentDef
	procs  map[string]*Agent
	// resumeDenied latches a Cursor session/load fail-closed so Launch
	// and the HTTP list path cannot stack another writer (🎯T541.1).
	// In-memory only — a process restart retries once.
	resumeDenied map[string]error
	// freshSession is name → SessionID assigned by Register/EnsureAgent
	// in this process. A persisted Grok/Codex/Cursor id loaded from disk is
	// not listed here, so bounce Launch RequireResume-es it (🎯T545.1).
	freshSession map[string]string
	lifecycle    map[string]*registryLifecycle
	// direct makes every launch take the in-process provider path even
	// when a daemon listens. The daemon's own registry is direct: it IS
	// the daemon, and dialling itself would wait on itself.
	direct bool
}

// NewRegistry loads or creates an agent registry at the given path.
// If the file does not exist, an empty registry is created.
func NewRegistry(path string) (*Registry, error) {
	r := &Registry{
		path:         path,
		agents:       make(map[string]*AgentDef),
		procs:        make(map[string]*Agent),
		resumeDenied: make(map[string]error),
		freshSession: make(map[string]string),
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var defs []AgentDef
		if err := json.Unmarshal(data, &defs); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for i := range defs {
			r.agents[defs[i].Name] = &defs[i]
		}
	}

	return r, nil
}

func (r *Registry) save() error {
	defs := make([]AgentDef, 0, len(r.agents))
	for _, d := range r.agents {
		defs = append(defs, cloneAgentDef(*d))
	}
	data, err := json.MarshalIndent(defs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, data, 0o644)
}

// Register adds or updates an agent definition and persists the registry.
// def.SessionID must be non-empty; use [Registry.EnsureAgent] if you want
// automatic session ID generation.
func (r *Registry) Register(def AgentDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerLocked(def)
}

func (r *Registry) registerLocked(def AgentDef) error {
	if def.SessionID == "" {
		return fmt.Errorf("agent %q: session_id required", def.Name)
	}
	old, existed := r.agents[def.Name]
	if op := r.lifecycle[def.Name]; op != nil && (!existed || !sameLaunchDefinition(*old, def)) {
		return fmt.Errorf("agent %q: %w", def.Name, ErrLifecycleInProgress)
	}
	def = cloneAgentDef(def)
	if existed && old.SessionID != def.SessionID {
		delete(r.resumeDenied, def.Name)
		r.freshSession[def.Name] = def.SessionID
	}
	if !existed {
		r.freshSession[def.Name] = def.SessionID
	}
	r.agents[def.Name] = &def
	return r.save()
}

// Remove removes an agent definition and stops it if running.
func (r *Registry) Remove(name string) error {
	return r.stopLifecycle(name, true)
}

// registryStart is the Session entrypoint used by [Registry.Launch].
// Production points at [StartContext]; hermetic tests may override it.
var registryStart = StartContext

// registryAdopt is the Session entrypoint used by [Registry.Adopt].
var registryAdopt = Adopt

// registryStartDirect is the Session entrypoint a direct-mode registry (the
// daemon's) uses. Hermetic tests point it at a fake backend.
var registryStartDirect = startDirectContext

// Launch starts the registered agent named name and returns it. If the agent
// is already running and alive, the existing [Agent] is returned without
// spawning a new process. It returns an error if name is not registered.
// MCP comes only from AgentDef.MCPServers / MCPExclusive — Launch does
// not scan workDir for mcp.claudia.json / .mcp.json and does not write
// provider HOME configs.
//
// Provider is taken from AgentDef.Provider (empty = Claude). When the
// launched agent reports a different SessionID (e.g. Grok ACP session/new),
// the definition is updated and persisted only when this Launch was
// allowed to mint. A remint under RequireResume is refused and the
// prior session_id stays on disk (jevons 🎯T545.1).
//
// Materialized is never set solely because Start succeeded. When Claude
// session JSONL already exists for the def's SessionID, Launch promotes
// Materialized so subsequent launches require resume. Hosts that complete
// a first turn after a bare Start should call [Registry.MarkMaterialized].
func (r *Registry) Launch(name string) (*Agent, error) {
	return r.LaunchContext(context.Background(), name)
}

// LaunchContext is Launch with cooperative startup cancellation. Its context
// does not own the returned agent. See StartContext for backend support.
func (r *Registry) LaunchContext(ctx context.Context, name string) (*Agent, error) {
	return r.startLifecycle(ctx, name, false, false)
}

// Adopt rebuilds a handle for a still-running process under the same per-name
// reservation as Launch. Cursor stdio leftovers cannot be adopted.
func (r *Registry) Adopt(name string) (*Agent, error) {
	return r.startLifecycle(context.Background(), name, true, false)
}

// AdoptOrLaunch adopts or starts inside one reservation, so no second caller
// can open a provider writer between the failed adopt and its fallback.
func (r *Registry) AdoptOrLaunch(name string) (*Agent, error) {
	return r.AdoptOrLaunchContext(context.Background(), name)
}

func (r *Registry) AdoptOrLaunchContext(ctx context.Context, name string) (*Agent, error) {
	return r.startLifecycle(ctx, name, true, true)
}

func (r *Registry) startLifecycle(ctx context.Context, name string, adopt, fallback bool) (*Agent, error) {
	ctx, op, finish, err := r.beginLifecycle(ctx, name, false)
	if err != nil {
		return nil, err
	}
	defer finish()
	r.mu.Lock()
	registered, ok := r.agents[name]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("agent %q not registered", name)
	}
	def := cloneAgentDef(*registered)
	wantResume := r.requireResumeLocked(&def)
	prior, denied := r.procs[name], r.resumeDenied[name]
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prior != nil && prior.Alive() {
		return prior, nil
	}
	if denied != nil {
		return nil, denied
	}
	cfg := registryConfig(&def, wantResume)
	var proc *Agent
	started := !adopt
	// A listening daemon holds the seat (🎯T2.10 / 🎯T2.11): adopt and
	// launch are one grant, and the daemon decides whether the process is
	// still there. Without a daemon the per-provider adopt logic below is
	// what survival looks like.
	if usingBroker() && !r.direct {
		proc, err = startViaBrokerContext(withGrantHint(ctx, grantHint{adopt: adopt, fallback: fallback, def: &def}), cfg)
		if err == nil || !brokerFellThrough(err) {
			adopt, started = false, true
		} else {
			proc, err = nil, nil
		}
	}
	if proc == nil && err == nil && adopt {
		switch {
		case isClaudeProvider(def.Provider):
			proc, err = registryAdopt(cfg)
		case def.Provider == ProviderGrok && (def.ConnectURL != "" || def.ConnectPID > 0):
			proc, err = registryStart(ctx, cfg)
		case def.Provider == ProviderCursor:
			reapCursorACPDef(&def)
			err = fmt.Errorf("%w: %s", ErrNoSessionWindow, def.SessionID)
		default:
			err = fmt.Errorf("%w: %s", ErrNoSessionWindow, def.SessionID)
		}
	}
	if proc == nil && (!adopt || (fallback && err != nil && ctx.Err() == nil)) {
		started = true
		if err != nil && !errors.Is(err, ErrNoSessionWindow) {
			slog.Warn("adopt failed; falling back to launch", "agent", name, "err", err)
		}
		if r.direct {
			proc, err = registryStartDirect(ctx, cfg)
		} else {
			proc, err = registryStart(ctx, cfg)
		}
	}
	r.mu.Lock()
	// Stop/Remove intent wins even if a backend ignored cancellation and
	// returned a process. Keep the reservation while that process is cleaned up.
	if op.stops > 0 {
		r.mu.Unlock()
		if proc != nil {
			proc.Stop()
		}
		return nil, context.Canceled
	}
	if err != nil {
		if IsCursorResumeDenied(err) {
			if r.resumeDenied == nil {
				r.resumeDenied = make(map[string]error)
			}
			r.resumeDenied[name] = err
		}
		r.mu.Unlock()
		return nil, err
	}
	if proc == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("agent %q: startup returned no process", name)
	}
	current := r.agents[name]
	if sid := proc.SessionID(); sid != "" && sid != def.SessionID && (wantResume || current.Materialized) {
		r.mu.Unlock()
		proc.Stop()
		return nil, fmt.Errorf("refusing remint of %s: session %s → %s — existing conversation required", name, def.SessionID, sid)
	}
	delete(r.resumeDenied, name)
	changed := false
	if sid := proc.SessionID(); sid != "" && sid != current.SessionID {
		current.SessionID = sid
		changed = true
	}
	if u := proc.ConnectURL(); u != current.ConnectURL {
		current.ConnectURL = u
		changed = true
	}
	if p := proc.PID(); p != current.ConnectPID {
		current.ConnectPID = p
		changed = true
	}
	if !current.Materialized && claudeSessionEvidence(current.Provider, current.SessionID, current.WorkDir) {
		current.Materialized = true
		changed = true
	}
	if changed {
		if err := r.save(); err != nil {
			slog.Warn("persist agent def after startup", "name", name, "err", err)
		}
	}
	r.procs[name] = proc
	materialized := current.Materialized
	r.mu.Unlock()
	message := "agent adopted"
	if started {
		message = "agent started"
	}
	slog.Info(message, "name", name, "provider", def.Provider, "session", proc.SessionID(),
		"connect_pid", proc.PID(), "connect_url_set", proc.ConnectURL() != "", "window", proc.WindowID(), "materialized", materialized)
	return proc, nil
}

// MarkMaterialized records that name has hosted a real conversation and
// persists Materialized=true so later launches pass RequireResume.
//
// For Claude (empty Provider), durable session JSONL must already exist
// ([SessionExists]); callers cannot flip the flag on bare process start.
// For other providers, the host attests first-turn / durable transcript
// evidence (no Claude JSONL path applies).
//
// Idempotent when already Materialized. Returns an error if name is
// unknown or Claude evidence is still missing.
func (r *Registry) MarkMaterialized(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	def, ok := r.agents[name]
	if !ok {
		return fmt.Errorf("agent %q not registered", name)
	}
	if def.Materialized {
		return nil
	}
	if isClaudeProvider(def.Provider) {
		if !claudeSessionEvidence(def.Provider, def.SessionID, def.WorkDir) {
			return fmt.Errorf("agent %q: cannot mark materialized without session JSONL (SessionID=%s)", name, def.SessionID)
		}
	}
	def.Materialized = true
	return r.save()
}

func isClaudeProvider(p Provider) bool {
	return p == "" || p == ProviderClaude
}

// requireResumeLocked reports whether Launch/Adopt must fail closed
// rather than mint a replacement session. Caller holds r.mu.
func (r *Registry) requireResumeLocked(def *AgentDef) bool {
	if def == nil {
		return false
	}
	if def.Materialized {
		return true
	}
	if def.SessionID == "" {
		return false
	}
	switch def.Provider {
	case ProviderGrok, ProviderCodex, ProviderCursor:
		// First mint this process (EnsureAgent / Register) may fall
		// through. A row reloaded from disk is a bounce resume.
		return r.freshSession[def.Name] != def.SessionID
	default:
		return false
	}
}

// claudeSessionEvidence reports whether Claude durable transcript evidence
// exists for sessionID under workDir. Non-Claude providers always return
// false here (they materialize via [Registry.MarkMaterialized] attestation).
func claudeSessionEvidence(provider Provider, sessionID, workDir string) bool {
	if !isClaudeProvider(provider) || sessionID == "" || workDir == "" {
		return false
	}
	ok, err := SessionExists(sessionID, workDir)
	return err == nil && ok
}

// Stop stops a running agent. For Grok connect-mode this kills the
// durable serve process and clears ConnectURL/ConnectPID so the next
// Launch does not reattach to a dead endpoint.
func (r *Registry) Stop(name string) {
	if err := r.stopLifecycle(name, false); err != nil {
		slog.Warn("persist agent stop", "name", name, "err", err)
	}
}

func (r *Registry) stopLifecycle(name string, remove bool) error {
	_, _, finish, err := r.beginLifecycle(context.Background(), name, true)
	if err != nil {
		return err
	}
	defer finish()
	r.mu.Lock()
	proc := r.procs[name]
	var def *AgentDef
	if current := r.agents[name]; current != nil {
		copy := cloneAgentDef(*current)
		def = &copy
	}
	r.mu.Unlock()
	if proc != nil {
		proc.Stop()
	} else if def != nil {
		reapSessionWindows(def)
		reapCursorACPDef(def)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.procs, name)
	if remove {
		delete(r.agents, name)
		delete(r.resumeDenied, name)
		delete(r.freshSession, name)
		return r.save()
	}
	if current := r.agents[name]; current != nil && (current.ConnectURL != "" || current.ConnectPID != 0) {
		current.ConnectURL, current.ConnectPID = "", 0
		return r.save()
	}
	return nil
}

// ResumeDenied returns the latched Cursor fail-closed error for name,
// or nil if Launch may still be attempted.
func (r *Registry) ResumeDenied(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resumeDenied == nil {
		return nil
	}
	return r.resumeDenied[name]
}

// Get returns the running agent for a name, or nil.
func (r *Registry) Get(name string) *Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[name]
}

// Def returns the definition for an agent, or nil.
func (r *Registry) Def(name string) *AgentDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.agents[name]; ok {
		cp := cloneAgentDef(*d)
		return &cp
	}
	return nil
}

// List returns all registered agent definitions.
func (r *Registry) List() []AgentDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	defs := make([]AgentDef, 0, len(r.agents))
	for _, d := range r.agents {
		defs = append(defs, cloneAgentDef(*d))
	}
	return defs
}

// StartAll starts all agents marked with AutoStart.
func (r *Registry) StartAll() {
	r.mu.Lock()
	names := make([]string, 0)
	for name, def := range r.agents {
		if def.AutoStart {
			names = append(names, name)
		}
	}
	r.mu.Unlock()

	for _, name := range names {
		if _, err := r.Launch(name); err != nil {
			slog.Error("auto-start failed", "agent", name, "err", err)
		}
	}
}

// StartAllPreferAdopt is the upgrade-boot counterpart of [Registry.StartAll]:
// reuse a leftover process when one exists, otherwise Launch. Ordinary
// start never reaps; a leak stays visible as extra processes.
func (r *Registry) StartAllPreferAdopt() {
	r.StartAllPreferAdoptContext(context.Background())
}

// StartAllPreferAdoptContext stops starting further seats when ctx is canceled.
func (r *Registry) StartAllPreferAdoptContext(ctx context.Context) {
	r.mu.Lock()
	names := make([]string, 0)
	for name, def := range r.agents {
		if def.AutoStart {
			names = append(names, name)
		}
	}
	r.mu.Unlock()

	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		if _, err := r.AdoptOrLaunchContext(ctx, name); err != nil {
			slog.Error("auto-start failed", "agent", name, "err", err)
		}
	}
}

// StopAll stops every registered agent, including those whose handles
// were lost when the previous consumer died. Walking defs rather than
// procs is what makes a clean jevonsd exit reap the fleet (🎯T34).
func (r *Registry) StopAll() {
	r.mu.Lock()
	names := make([]string, 0, len(r.agents))
	for name := range r.agents {
		names = append(names, name)
	}
	r.mu.Unlock()

	for _, name := range names {
		r.Stop(name)
	}
}

// reapSessionWindows kills leftover Claude tmux windows for def's
// session. Grok connect-mode processes are reached via ConnectPID on
// the next Launch, not here.
func reapSessionWindows(def *AgentDef) {
	if def == nil || !isClaudeProvider(def.Provider) || def.SessionID == "" {
		return
	}
	if err := tmuxagent.KillWindowsForSession(def.SessionID); err != nil {
		slog.Warn("reap session windows", "name", def.Name, "session", def.SessionID, "err", err)
	}
}

// EnsureAgent returns the existing [AgentDef] for name if it is already
// registered (same-name idempotent). If the name is new, it always mints a
// fresh def and SessionID — even when another agent already uses the same
// workDir. Identity is name-keyed; multiple concurrent workers may share a
// repo path without stealing each other's session or process.
//
// parent is recorded only when minting a new agent; an existing def keeps
// its Parent (callers that must reparent should Register explicitly).
func (r *Registry) EnsureAgent(name, workDir, model string, autoStart bool) (*AgentDef, error) {
	return r.EnsureAgentWithParent(name, workDir, model, "", autoStart)
}

// EnsureAgentWithParent is [EnsureAgent] with an explicit parent name for
// new registrations (fleet lineage for kill authorization).
func (r *Registry) EnsureAgentWithParent(name, workDir, model, parent string, autoStart bool) (*AgentDef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if def, ok := r.agents[name]; ok {
		cp := cloneAgentDef(*def)
		return &cp, nil
	}

	def := AgentDef{
		Name:      name,
		WorkDir:   workDir,
		SessionID: uuid.New().String(),
		Model:     model,
		AutoStart: autoStart,
		Parent:    parent,
	}
	if err := r.registerLocked(def); err != nil {
		return nil, err
	}
	return &def, nil
}

// IsAncestor reports whether ancestor is a strict ancestor of name by
// walking Parent links. Cycles are broken with a visit set.
func (r *Registry) IsAncestor(ancestor, name string) bool {
	if ancestor == "" || name == "" || ancestor == name {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	cur := name
	for cur != "" && !seen[cur] {
		seen[cur] = true
		def, ok := r.agents[cur]
		if !ok || def.Parent == "" {
			return false
		}
		if def.Parent == ancestor {
			return true
		}
		cur = def.Parent
	}
	return false
}

// Descendants returns names of all agents in the subtree under root
// (not including root), depth-first.
func (r *Registry) Descendants(root string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	var walk func(string)
	walk = func(parent string) {
		for name, def := range r.agents {
			if def.Parent == parent {
				out = append(out, name)
				walk(name)
			}
		}
	}
	walk(root)
	return out
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	Materialized bool `json:"materialized,omitempty"`

	// Provider selects the runtime (claude, codex, grok). Empty means
	// ProviderClaude. Grok Session uses ACP over `grok agent stdio`.
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

	// Goal is the durable host-owned Session objective (🎯T39). Copied
	// onto Config.Goal at Launch/Adopt so a provider switch keeps the
	// same objective. Empty means one-shot Send.
	Goal string `json:"goal,omitempty"`
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
}

// NewRegistry loads or creates an agent registry at the given path.
// If the file does not exist, an empty registry is created.
func NewRegistry(path string) (*Registry, error) {
	r := &Registry{
		path:   path,
		agents: make(map[string]*AgentDef),
		procs:  make(map[string]*Agent),
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
		defs = append(defs, *d)
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

	if def.SessionID == "" {
		return fmt.Errorf("agent %q: session_id required", def.Name)
	}
	r.agents[def.Name] = &def
	return r.save()
}

// Remove removes an agent definition and stops it if running.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if proc, ok := r.procs[name]; ok {
		proc.Stop()
		delete(r.procs, name)
	} else if def, ok := r.agents[name]; ok {
		reapSessionWindows(def)
	}
	delete(r.agents, name)
	return r.save()
}

// registryStart is the Session entrypoint used by [Registry.Launch].
// Production points at [Start]; hermetic tests may override it.
var registryStart = Start

// registryAdopt is the Session entrypoint used by [Registry.Adopt].
var registryAdopt = Adopt

// Launch starts the registered agent named name and returns it. If the agent
// is already running and alive, the existing [Agent] is returned without
// spawning a new process. It returns an error if name is not registered.
// If the agent's workDir contains a .mcp.json file, it is passed to the
// provider when supported (Claude MCP config path).
//
// Provider is taken from AgentDef.Provider (empty = Claude). When the
// launched agent reports a different SessionID (e.g. Grok ACP session/new),
// the definition is updated and persisted.
//
// Materialized is never set solely because Start succeeded. When Claude
// session JSONL already exists for the def's SessionID, Launch promotes
// Materialized so subsequent launches require resume. Hosts that complete
// a first turn after a bare Start should call [Registry.MarkMaterialized].
func (r *Registry) Launch(name string) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if proc, ok := r.procs[name]; ok && proc.Alive() {
		return proc, nil
	}

	def, ok := r.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %q not registered", name)
	}

	// Prefer mcp.claudia.json: a file the Grok CLI does not scan. Entries
	// in cwd/.mcp.json are cross-referenced by the CLI and classified as
	// repo-local, which silently gates them behind folder trust — the
	// claudia-private filename keeps ACP-passed servers session-scoped.
	mcpConfig := filepath.Join(def.WorkDir, "mcp.claudia.json")
	if _, err := os.Stat(mcpConfig); err != nil {
		mcpConfig = filepath.Join(def.WorkDir, ".mcp.json")
		if _, err := os.Stat(mcpConfig); err != nil {
			mcpConfig = ""
		}
	}

	proc, err := registryStart(Config{
		Provider:      def.Provider,
		WorkDir:       def.WorkDir,
		SessionID:     def.SessionID,
		RequireResume: def.Materialized,
		Model:         def.Model,
		DisallowTools: def.DisallowTools,
		MCPConfig:     mcpConfig,
		GrokConnect:   def.GrokConnect || def.ConnectURL != "",
		ConnectURL:    def.ConnectURL,
		ConnectPID:    def.ConnectPID,
		SandboxMode:   def.SandboxMode,
		Goal:          def.Goal,
	})
	if err != nil {
		return nil, err
	}

	changed := false
	if sid := proc.SessionID(); sid != "" && sid != def.SessionID {
		def.SessionID = sid
		changed = true
	}
	if u := proc.ConnectURL(); u != def.ConnectURL {
		def.ConnectURL = u
		changed = true
	}
	if p := proc.PID(); p != def.ConnectPID {
		def.ConnectPID = p
		changed = true
	}
	// Promote Materialized only from conversation evidence, never from
	// bare Start success (Claude: SessionExists / durable JSONL).
	if !def.Materialized && claudeSessionEvidence(def.Provider, def.SessionID, def.WorkDir) {
		def.Materialized = true
		changed = true
	}
	if changed {
		if err := r.save(); err != nil {
			slog.Warn("persist agent def after launch", "name", name, "err", err)
		}
	}

	r.procs[name] = proc
	slog.Info("agent started", "name", name, "provider", def.Provider, "session", proc.SessionID(),
		"connect_pid", proc.PID(), "connect_url_set", proc.ConnectURL() != "",
		"window", proc.WindowID(),
		"materialized", def.Materialized)
	return proc, nil
}

// Adopt rebuilds a handle for a still-running process. It does not
// spawn and does not reap. Drain boot uses [Registry.Launch]; upgrade
// boot uses [Registry.AdoptOrLaunch].
func (r *Registry) Adopt(name string) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if proc, ok := r.procs[name]; ok && proc.Alive() {
		return proc, nil
	}

	def, ok := r.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %q not registered", name)
	}

	mcpConfig := filepath.Join(def.WorkDir, "mcp.claudia.json")
	if _, err := os.Stat(mcpConfig); err != nil {
		mcpConfig = filepath.Join(def.WorkDir, ".mcp.json")
		if _, err := os.Stat(mcpConfig); err != nil {
			mcpConfig = ""
		}
	}

	cfg := Config{
		Provider:      def.Provider,
		WorkDir:       def.WorkDir,
		SessionID:     def.SessionID,
		RequireResume: def.Materialized,
		Model:         def.Model,
		DisallowTools: def.DisallowTools,
		MCPConfig:     mcpConfig,
		GrokConnect:   def.GrokConnect || def.ConnectURL != "",
		ConnectURL:    def.ConnectURL,
		ConnectPID:    def.ConnectPID,
		SandboxMode:   def.SandboxMode,
		Goal:          def.Goal,
	}

	var proc *Agent
	var err error
	switch {
	case isClaudeProvider(def.Provider):
		proc, err = registryAdopt(cfg)
	case def.ConnectURL != "" || def.ConnectPID > 0:
		// Grok connect-mode: Start dials the existing serve.
		proc, err = registryStart(cfg)
	default:
		err = fmt.Errorf("%w: %s", ErrNoSessionWindow, def.SessionID)
	}
	if err != nil {
		return nil, err
	}

	changed := false
	if sid := proc.SessionID(); sid != "" && sid != def.SessionID {
		def.SessionID = sid
		changed = true
	}
	if u := proc.ConnectURL(); u != def.ConnectURL {
		def.ConnectURL = u
		changed = true
	}
	if p := proc.PID(); p != def.ConnectPID {
		def.ConnectPID = p
		changed = true
	}
	if !def.Materialized && claudeSessionEvidence(def.Provider, def.SessionID, def.WorkDir) {
		def.Materialized = true
		changed = true
	}
	if changed {
		if err := r.save(); err != nil {
			slog.Warn("persist agent def after adopt", "name", name, "err", err)
		}
	}

	r.procs[name] = proc
	slog.Info("agent adopted", "name", name, "provider", def.Provider, "session", proc.SessionID(),
		"connect_pid", proc.PID(), "window", proc.WindowID())
	return proc, nil
}

// AdoptOrLaunch tries [Registry.Adopt] and falls back to [Registry.Launch]
// when no process remains. Only the upgrade boot uses this.
func (r *Registry) AdoptOrLaunch(name string) (*Agent, error) {
	proc, err := r.Adopt(name)
	if err == nil {
		return proc, nil
	}
	if !errors.Is(err, ErrNoSessionWindow) {
		slog.Warn("adopt failed; falling back to launch", "agent", name, "err", err)
	}
	return r.Launch(name)
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
	r.mu.Lock()
	defer r.mu.Unlock()

	if proc, ok := r.procs[name]; ok {
		proc.Stop()
		delete(r.procs, name)
		slog.Info("agent stopped", "name", name)
	} else if def, ok := r.agents[name]; ok {
		// After a consumer restart procs is empty. The window is still
		// findable by session id; killing it is what makes Stop a real
		// cleanup rather than a silent no-op (🎯T34).
		reapSessionWindows(def)
		slog.Info("agent stopped", "name", name, "via", "session-window")
	}
	if def, ok := r.agents[name]; ok {
		if def.ConnectURL != "" || def.ConnectPID != 0 {
			def.ConnectURL = ""
			def.ConnectPID = 0
			if err := r.save(); err != nil {
				slog.Warn("persist clear connect endpoint after stop", "name", name, "err", err)
			}
		}
	}
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
		cp := *d
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
		defs = append(defs, *d)
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
	r.mu.Lock()
	names := make([]string, 0)
	for name, def := range r.agents {
		if def.AutoStart {
			names = append(names, name)
		}
	}
	r.mu.Unlock()

	for _, name := range names {
		if _, err := r.AdoptOrLaunch(name); err != nil {
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
	if def, ok := r.agents[name]; ok {
		cp := *def
		r.mu.Unlock()
		return &cp, nil
	}
	r.mu.Unlock()

	def := AgentDef{
		Name:      name,
		WorkDir:   workDir,
		SessionID: uuid.New().String(),
		Model:     model,
		AutoStart: autoStart,
		Parent:    parent,
	}
	if err := r.Register(def); err != nil {
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

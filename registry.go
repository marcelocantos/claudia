// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

// AgentDef is the persistent definition of an agent.
type AgentDef struct {
	Name          string `json:"name"`                      // unique identifier
	WorkDir       string `json:"workdir"`                   // working directory
	SessionID     string `json:"session_id"`                // persistent Claude session ID
	Model         string `json:"model,omitempty"`           // model override
	AutoStart     bool   `json:"auto_start"`                // start on registry startup
	Parent        string `json:"parent,omitempty"`          // parent agent name (for tree display)
	DisallowTools string `json:"disallow_tools,omitempty"`  // extra tools to disallow
}

// Registry manages persistent agent definitions and their running processes.
// Agent definitions are persisted to a JSON file and can be started/stopped
// independently of the registry lifecycle.
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

// Register adds or updates an agent definition and saves to disk.
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
	}
	delete(r.agents, name)
	return r.save()
}

// Start starts a registered agent. Returns the existing process if
// already running.
func (r *Registry) Start(name string) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if proc, ok := r.procs[name]; ok && proc.Alive() {
		return proc, nil
	}

	def, ok := r.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %q not registered", name)
	}

	mcpConfig := filepath.Join(def.WorkDir, ".mcp.json")
	if _, err := os.Stat(mcpConfig); err != nil {
		mcpConfig = ""
	}

	proc, err := Start(Config{
		WorkDir:       def.WorkDir,
		SessionID:     def.SessionID,
		Model:         def.Model,
		DisallowTools: def.DisallowTools,
		MCPConfig:     mcpConfig,
	})
	if err != nil {
		return nil, err
	}

	r.procs[name] = proc
	slog.Info("agent started", "name", name, "session", proc.SessionID())
	return proc, nil
}

// Stop stops a running agent.
func (r *Registry) Stop(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if proc, ok := r.procs[name]; ok {
		proc.Stop()
		delete(r.procs, name)
		slog.Info("agent stopped", "name", name)
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
		if _, err := r.Start(name); err != nil {
			slog.Error("auto-start failed", "agent", name, "err", err)
		}
	}
}

// StopAll stops all running agents.
func (r *Registry) StopAll() {
	r.mu.Lock()
	names := make([]string, 0, len(r.procs))
	for name := range r.procs {
		names = append(names, name)
	}
	r.mu.Unlock()

	for _, name := range names {
		r.Stop(name)
	}
}

// EnsureAgent registers an agent if it doesn't exist. If an agent
// with the same workdir exists under a different name, it is renamed.
// Returns the definition (existing or new).
func (r *Registry) EnsureAgent(name, workDir, model string, autoStart bool) (*AgentDef, error) {
	r.mu.Lock()
	if def, ok := r.agents[name]; ok {
		r.mu.Unlock()
		return def, nil
	}

	// Check if an agent with the same workdir exists under a different name.
	for oldName, def := range r.agents {
		if def.WorkDir == workDir {
			slog.Info("renaming agent", "from", oldName, "to", name)
			delete(r.agents, oldName)
			def.Name = name
			r.agents[name] = def
			if proc, ok := r.procs[oldName]; ok {
				delete(r.procs, oldName)
				r.procs[name] = proc
			}
			err := r.save()
			r.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return def, nil
		}
	}
	r.mu.Unlock()

	def := AgentDef{
		Name:      name,
		WorkDir:   workDir,
		SessionID: uuid.New().String(),
		Model:     model,
		AutoStart: autoStart,
	}
	if err := r.Register(def); err != nil {
		return nil, err
	}
	return &def, nil
}

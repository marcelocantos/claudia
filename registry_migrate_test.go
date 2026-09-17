// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"path/filepath"
	"testing"
)

// TestRegistryRecordsMigrate is 🎯T75.3: a direct-mode Registry, with no
// daemon, persists the backend a seat moved to, so a Registry reopened from
// disk names the destination rather than the source the seat left.
func TestRegistryRecordsMigrate(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		return startWithBackend(cfg, &fakeAgentBackend{name: "fake-claude"})
	}

	path := filepath.Join(t.TempDir(), "agents.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name: "seat", Provider: ProviderClaude, WorkDir: t.TempDir(),
		SessionID: "src-session", Model: "src-model", Materialized: false,
	}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)

	proc.PublishEvent(Event{Type: "user", Text: "patch registry.go"})
	proc.PublishEvent(Event{Type: "assistant", Text: "working on registry.go"})
	dest := &fakeAgentBackend{name: "fake-grok", assignedSession: "grok-dest-1"}
	if err := proc.migrateWithBackend(&MigrateArgs{Provider: ProviderGrok, Model: "grok-4"}, dest); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	def := reopened.Def("seat")
	if def == nil {
		t.Fatal("seat missing after reopen")
	}
	if def.Provider != ProviderGrok || def.SessionID != "grok-dest-1" || def.Model != "grok-4" {
		t.Fatalf("reopened def names %s / %s / %s, want the migrate destination", def.Provider, def.SessionID, def.Model)
	}
	if def.Materialized {
		t.Fatal("destination is a new native session; Materialized must be cleared")
	}
}

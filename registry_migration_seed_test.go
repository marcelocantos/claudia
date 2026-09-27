// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

type failedMigrationSeedBackend struct{ *fakeAgentBackend }

func (b failedMigrationSeedBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	start, err := b.fakeAgentBackend.StartAgent(req)
	if err != nil {
		return nil, err
	}
	start.Ops.send = func(*Agent, string) error { return errors.New("seed transport failed") }
	return start, nil
}

func TestRegistryReplaysPersistedMigrationSeedAfterRestart(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	path := filepath.Join(t.TempDir(), "agents.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name: "seat", Provider: ProviderClaude, WorkDir: t.TempDir(), SessionID: "source",
	}); err != nil {
		t.Fatal(err)
	}
	sourceBackend := &fakeAgentBackend{name: "source"}
	reg.SetLaunchers(&RegistryLaunchers{Start: func(_ context.Context, cfg Config) (*Agent, error) {
		return startWithBackend(cfg, sourceBackend)
	}})
	proc, err := reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	proc.PublishEvent(Event{Type: "user", Text: "remember violet"})
	destBackend := failedMigrationSeedBackend{&fakeAgentBackend{name: "destination", assignedSession: "dest-session"}}
	err = proc.migrateWithBackend(&MigrateArgs{
		Provider: ProviderGrok, Model: "grok-4", ContextBrief: "violet handover",
	}, destBackend)
	if err == nil || !strings.Contains(err.Error(), "seed send failed") {
		t.Fatalf("migration returned %v, want a failed seed send", err)
	}
	proc.Stop()

	restarted, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	def := restarted.Def("seat")
	if def == nil || def.Provider != ProviderGrok || def.SessionID != "dest-session" ||
		!strings.Contains(def.MigrationSeed, "violet handover") {
		t.Fatalf("destination and handover were not persisted together: %+v", def)
	}
	replayBackend := &fakeAgentBackend{name: "reopened", assignedSession: "dest-session"}
	restarted.SetLaunchers(&RegistryLaunchers{Start: func(_ context.Context, cfg Config) (*Agent, error) {
		return startWithBackend(cfg, replayBackend)
	}})
	reopened, err := restarted.AdoptOrLaunch("seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Stop)
	if reopened.SessionID() != "dest-session" || len(replayBackend.sends) != 1 ||
		!strings.Contains(replayBackend.sends[0], "violet handover") {
		t.Fatalf("handover replay moved session or missed seed: session=%q sends=%v", reopened.SessionID(), replayBackend.sends)
	}
	if _, err := restarted.AdoptOrLaunch("seat"); err != nil {
		t.Fatal(err)
	}
	if len(replayBackend.sends) != 1 {
		t.Fatalf("repeated adopt sent the handover twice: %v", replayBackend.sends)
	}
	reloaded, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Def("seat"); got == nil || got.MigrationSeed != "" || got.SessionID != "dest-session" {
		t.Fatalf("handover delivery was not durably cleared: %+v", got)
	}
}

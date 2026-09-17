// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
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
	dest := &fakeAgentBackend{name: "fake-grok", assignedSession: "grok-dest-1", connectURL: "ws://127.0.0.1:9/ws", connectPID: 4242}
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
	if def.ConnectURL != "ws://127.0.0.1:9/ws" || def.ConnectPID != 4242 {
		t.Fatalf("reopened def connect endpoint = %q / %d, want the destination's", def.ConnectURL, def.ConnectPID)
	}
	if def.Materialized {
		t.Fatal("destination is a new native session; Materialized must be cleared")
	}
}

// migrateOnlyDaemon answers grant, migrate and release like a daemon whose
// seat moves to Grok, and nothing else: enough to drive a consumer's
// daemon-held handle through Migrate.
type migrateOnlyDaemon struct{}

func (migrateOnlyDaemon) HandleRequest(c *broker.ClientConn, req *broker.Request) bool {
	switch req.Type {
	case broker.TypeGrant:
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGranted, Granted: &broker.GrantResponse{
			Name: req.Grant.Name, SessionID: "src-session", Provider: broker.ProviderClaude, Model: "src-model",
		}})
	case broker.TypeMigrate:
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeMigrated, Migrated: &broker.MigrateResponse{
			Name: req.Migrate.Name, SessionID: "grok-dest-2", Provider: broker.Provider(req.Migrate.Provider), Model: req.Migrate.Model,
		}})
	case broker.TypeRelease:
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeReleased, Released: &broker.ReleaseResponse{
			Name: req.Release.Name, Disposition: req.Release.Disposition,
		}})
	default:
		return false
	}
	return true
}

func (migrateOnlyDaemon) ConnClosed(*broker.ClientConn) {}

// TestRegistryRecordsMigrateOnDaemonHeldSeat is 🎯T75.3's broker-handle
// branch: a consumer Registry whose seat a daemon holds records the
// destination the daemon reports for Migrate, so the consumer's own
// definition does not keep naming the source.
func TestRegistryRecordsMigrateOnDaemonHeldSeat(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	dir, err := os.MkdirTemp("/tmp", "crm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	ln, err := broker.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := broker.Serve(&broker.ServeArgs{Listener: ln, Handler: migrateOnlyDaemon{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	path := filepath.Join(t.TempDir(), "agents.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "held", Provider: ProviderClaude, WorkDir: t.TempDir(), SessionID: "src-session", Model: "src-model"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("held")
	if err != nil {
		t.Fatal(err)
	}
	if !proc.DaemonHeld() {
		t.Fatal("seat is not daemon-held")
	}
	if err := proc.Migrate(&MigrateArgs{Provider: ProviderGrok, Model: "grok-4"}); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	def := reopened.Def("held")
	if def == nil || def.Provider != ProviderGrok || def.SessionID != "grok-dest-2" || def.Model != "grok-4" || def.Materialized {
		t.Fatalf("reopened def = %+v, want the daemon's migrate destination", def)
	}
}

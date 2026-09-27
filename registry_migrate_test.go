// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestRegistryDoesNotLaunchOverMismatchedSidecarSeat(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "seat", Provider: "xai-oauth", WorkDir: t.TempDir(), SessionID: "source"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	reg.SetLaunchers(&RegistryLaunchers{Start: func(_ context.Context, cfg Config) (*Agent, error) {
		calls++
		if !cfg.AdoptOnly {
			t.Fatal("registry launched over a seat whose provider differed")
		}
		return nil, ErrSeatIdentityMismatch
	}})
	_, err = reg.AdoptOrLaunch("seat")
	if !errors.Is(err, ErrSeatIdentityMismatch) || calls != 1 {
		t.Fatalf("adopt outcome err=%v calls=%d; want mismatch without launch", err, calls)
	}
}

func TestRegistryMigrateDoesNotReportAnUnpersistedDestination(t *testing.T) {
	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		return startWithBackend(cfg, &fakeAgentBackend{name: "fake-claude"})
	}
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "seat", Provider: ProviderClaude, WorkDir: t.TempDir(), SessionID: "source"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)
	proc.PublishEvent(Event{Type: "user", Text: "remember violet"})
	proc.PublishEvent(Event{Type: "assistant", Text: "working"})
	path := reg.path
	reg.path = filepath.Join(t.TempDir(), "missing", "agents.json")
	dest := &fakeAgentBackend{name: "fake-grok", assignedSession: "destination"}
	err = proc.migrateWithBackend(&MigrateArgs{Provider: ProviderGrok, ContextBrief: "violet"},
		dest)
	if err == nil || !strings.Contains(err.Error(), "registry persistence failed") {
		t.Fatalf("unpersisted migration returned %v", err)
	}
	if got := reg.Def("seat"); got == nil || got.Provider != ProviderClaude || got.SessionID != "source" {
		t.Fatalf("registry claimed an unpersisted destination: %+v", got)
	}
	if proc.Provider() != ProviderGrok {
		t.Fatalf("destination was not started; test did not cross persistence boundary: %s", proc.Provider())
	}
	reg.path = path
	if err := proc.migrateWithBackend(&MigrateArgs{Provider: ProviderGrok, ContextBrief: "violet"}, dest); err != nil {
		t.Fatalf("retry must persist the already-running destination: %v", err)
	}
	if len(dest.requests) != 1 {
		t.Fatalf("retry started another destination: %d starts", len(dest.requests))
	}
	if len(dest.sends) != 1 || !strings.Contains(dest.sends[0], "violet") {
		t.Fatalf("retry did not send the prepared brief exactly once: %v", dest.sends)
	}
	reopened, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Def("seat"); got == nil || got.Provider != ProviderGrok || got.SessionID != "destination" {
		t.Fatalf("retry did not persist the destination: %+v", got)
	}
}

// migrateOnlyDaemon answers grant, migrate and release like a daemon whose
// seat moves to Grok, and nothing else: enough to drive a consumer's
// daemon-held handle through Migrate.
type migrateOnlyDaemon struct {
	t *testing.T
}

func (d migrateOnlyDaemon) HandleRequest(c *broker.ClientConn, req *broker.Request) bool {
	switch req.Type {
	case broker.TypeGrant:
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGranted, Granted: &broker.GrantResponse{
			Name: req.Grant.Name, SessionID: "src-session", Provider: broker.ProviderClaude, Model: "src-model",
		}})
	case broker.TypeMigrate:
		if req.Migrate.ContextBrief != "predecessor decision: violet" {
			d.t.Errorf("prepared context brief was lost on broker wire: %q", req.Migrate.ContextBrief)
		}
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
	srv, err := broker.Serve(&broker.ServeArgs{Listener: ln, Handler: migrateOnlyDaemon{t: t}})
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
	if err := proc.Migrate(&MigrateArgs{Provider: ProviderGrok, Model: "grok-4", ContextBrief: "predecessor decision: violet"}); err != nil {
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

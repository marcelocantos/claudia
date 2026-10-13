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
	"github.com/marcelocantos/claudia/omp"
)

// switchRegistry is a registry on a file, whose seats launch directly on the
// fake sidecar.
func switchRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetLaunchers(&RegistryLaunchers{Start: func(ctx context.Context, cfg Config) (*Agent, error) {
		cfg.TermLogPath = "-"
		return startDirectContext(ctx, cfg)
	}})
	return reg, path
}

// 🎯T195: a running seat switches in place, and its definition follows it,
// on disk, keeping its session.
func TestT195RegistrySwitchesALiveSeatAndRecordsIt(t *testing.T) {
	fake := startFakeSidecar(t, nil)
	reg, path := switchRegistry(t)
	if err := reg.Register(AgentDef{Name: "w", Provider: Provider(omp.Anthropic), Model: "claude-opus", WorkDir: t.TempDir(), SessionID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("w")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Stop("w")
	if err := proc.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := reg.SwitchModel(context.Background(), "w", ProviderCodex, "gpt-5.5", false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Live || !got.Changed || got.From != Provider(omp.Anthropic) || got.To != Provider(omp.OpenAICodex) {
		t.Fatalf("switch = %+v", got)
	}
	if loads := fake.loaded(); len(loads) != 2 || loads[1].Provider != omp.OpenAICodex {
		t.Fatalf("loads = %+v, want the launch and a switch to openai-codex", loads)
	}
	reread, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	def := reread.Def("w")
	if def.Provider != Provider(omp.OpenAICodex) || def.Model != "gpt-5.5" || def.SessionID != "s-1" {
		t.Fatalf("persisted def = %s/%s session %s, want openai-codex/gpt-5.5 on session s-1", def.Provider, def.Model, def.SessionID)
	}

	// Already there: nothing to do.
	again, err := reg.SwitchModel(context.Background(), "w", "openai-codex", "gpt-5.5", false)
	if err != nil || again.Changed {
		t.Fatalf("repeat switch = %+v, %v; want unchanged", again, err)
	}
}

// A seat that is not running has its definition rewritten on the same
// session; its next launch resumes the conversation on the new model.
func TestT195RegistrySwitchesAStoppedSeatOnItsSession(t *testing.T) {
	reg, path := switchRegistry(t)
	if err := reg.Register(AgentDef{Name: "w", Provider: Provider(omp.Anthropic), Model: "claude-opus", SessionID: "s-1", Materialized: true}); err != nil {
		t.Fatal(err)
	}
	got, err := reg.SwitchModel(context.Background(), "w", "openai-codex", "gpt-5.5", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Live || !got.Changed {
		t.Fatalf("switch = %+v, want a stopped seat rewritten", got)
	}
	reread, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	def := reread.Def("w")
	if def.Provider != "openai-codex" || def.Model != "gpt-5.5" || def.SessionID != "s-1" || !def.Materialized {
		t.Fatalf("persisted def = %+v, want openai-codex/gpt-5.5 on the same materialized session", def)
	}
}

func TestT195RegistrySwitchRefusals(t *testing.T) {
	reg, _ := switchRegistry(t)
	if _, err := reg.SwitchModel(context.Background(), "ghost", "anthropic", "m", false); err == nil {
		t.Fatal("switching an unregistered seat did not fail")
	}
	if err := reg.Register(AgentDef{Name: "local", Provider: ProviderOllama, Model: "llama3", SessionID: "s-local"}); err != nil {
		t.Fatal(err)
	}
	_, err := reg.SwitchModel(context.Background(), "local", "anthropic", "claude-opus", false)
	if err == nil || !strings.Contains(err.Error(), "only a sidecar seat changes provider") {
		t.Fatalf("err = %v, want a refusal for a non-sidecar seat changing provider", err)
	}
	if def := reg.Def("local"); def.Provider != ProviderOllama {
		t.Fatalf("refused switch rewrote the seat: %+v", def)
	}
}

// grantsBroker answers a grants listing with the seats it is running.
type grantsBroker struct{ live []string }

func (b grantsBroker) HandleRequest(c *broker.ClientConn, req *broker.Request) bool {
	if req.Type != broker.TypeGrants {
		return false
	}
	var grants []broker.GrantStatus
	for _, name := range b.live {
		grants = append(grants, broker.GrantStatus{Name: name, Provider: "anthropic", Alive: true})
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGrantsResult, Grants: &broker.GrantsResponse{Grants: grants}})
	return true
}

func (grantsBroker) ConnClosed(*broker.ClientConn) {}

// A seat the broker is running but this host has not attached cannot be
// switched by rewriting its definition: the running seat would not hear it,
// and the adopt that follows would put the old model back.
func TestT195StoppedSwitchRefusesASeatTheBrokerRuns(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "claudia-switch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "broker.sock")
	t.Setenv(broker.SocketPathEnv, socket)
	t.Setenv(broker.NoBrokerEnv, "0")
	listener, err := broker.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := broker.Serve(&broker.ServeArgs{Listener: listener, Handler: grantsBroker{live: []string{"held"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"held", "down"} {
		if err := reg.Register(AgentDef{Name: name, Provider: Provider(omp.Anthropic), Model: "claude-opus", SessionID: "s-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.SwitchModel(context.Background(), "held", "openai-codex", "gpt-5.5", false); !errors.Is(err, ErrSeatNotAttached) {
		t.Fatalf("err = %v, want ErrSeatNotAttached", err)
	}
	if def := reg.Def("held"); def.Provider != Provider(omp.Anthropic) || def.Model != "claude-opus" {
		t.Fatalf("refused switch rewrote the seat: %+v", def)
	}
	if got, err := reg.SwitchModel(context.Background(), "down", "openai-codex", "gpt-5.5", false); err != nil || !got.Changed {
		t.Fatalf("switch of a seat the broker is not running = %+v, %v", got, err)
	}
}

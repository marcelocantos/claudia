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

func TestStoppedMigrationPersistsOneTransferAcrossFailedLaunch(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name: "worker", Provider: ProviderGrok, SessionID: "source", Goal: "finish the mission", WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	summaries := 0
	r.migrationSummarizer = func(_ context.Context, args MigrationTransferArgs) (MigrationTransferResult, error) {
		summaries++
		if PlanProvider(args.Destination) != ProviderClaude || args.Goal != "finish the mission" ||
			!strings.Contains(args.Transcript, "violet") {
			t.Fatalf("transfer args = %+v", args)
		}
		return MigrationTransferResult{Brief: "Continue the violet mission."}, nil
	}
	starts := 0
	destinationBackend := &fakeAgentBackend{name: "destination"}
	r.SetLaunchers(&RegistryLaunchers{
		Adopt: func(Config) (*Agent, error) { return nil, ErrNoSessionWindow },
		Start: func(_ context.Context, cfg Config) (*Agent, error) {
			if cfg.AdoptOnly {
				return nil, ErrNoSessionWindow
			}
			starts++
			if starts == 1 {
				return nil, errors.New("destination temporarily unavailable")
			}
			return startWithBackend(cfg, destinationBackend)
		},
	})
	args := MigrateArgs{Provider: ProviderClaude}
	first, err := r.MigrateStopped(context.Background(), "worker", args, "user: remember violet")
	if err == nil || !strings.Contains(err.Error(), "launch failed") {
		t.Fatalf("first attempt = %+v, %v; want persisted launch failure", first, err)
	}
	if summaries != 1 || PlanProvider(first.Destination.Provider) != ProviderClaude || first.Destination.SessionID == "" || first.Destination.Model == "" {
		t.Fatalf("first attempt lost the prepared destination: %+v summaries=%d", first, summaries)
	}
	reopened, err := NewRegistry(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if def := reopened.Def("worker"); def == nil || !def.MigrationPendingStart ||
		!strings.Contains(def.MigrationSeed, "violet") || def.SessionID != first.Destination.SessionID {
		t.Fatalf("pending destination not durable: %+v", def)
	}
	second, err := r.MigrateStopped(context.Background(), "worker", args, "a different transcript must not be summarized")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Agent.Stop)
	if summaries != 1 || starts != 2 || second.Destination.SessionID != first.Destination.SessionID ||
		len(destinationBackend.sends) != 1 || !strings.Contains(destinationBackend.sends[0], "violet") {
		t.Fatalf("retry did not resume the one transfer: summaries=%d starts=%d result=%+v sends=%v",
			summaries, starts, second, destinationBackend.sends)
	}
	final, err := NewRegistry(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if def := final.Def("worker"); def == nil || def.MigrationPendingStart || def.MigrationSeed != "" ||
		def.SessionID != first.Destination.SessionID {
		t.Fatalf("completed destination not durable: %+v", def)
	}
}

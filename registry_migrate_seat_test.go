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

// TestRegistryMigrateLiveSeatSuccess (🎯T1013.5): the single decide-and-
// execute entry point moves a live seat in place without the caller
// orchestrating a separate prepare/thin-brief/seed/launch sequence.
func TestRegistryMigrateLiveSeatSuccess(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		return startWithBackend(cfg, &fakeAgentBackend{name: "fake-claude"})
	}
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name: "seat", Provider: ProviderClaude, WorkDir: t.TempDir(),
		SessionID: "src-session", Model: "src-model",
	}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)
	proc.PublishEvent(Event{Type: "user", Text: "patch migrate_seat.go"})

	var migrateCalls int
	prevLive := migrateLive
	t.Cleanup(func() { migrateLive = prevLive })
	migrateLive = func(a *Agent, args *MigrateArgs) error {
		migrateCalls++
		if PlanProvider(args.Provider) != PlanProvider(ProviderGrok) {
			t.Fatalf("migrateLive provider = %q", args.Provider)
		}
		// Simulate what the real Agent.Migrate does: swap the registry
		// row to the destination via notifyMigrated.
		next := AgentDef{Name: "seat", Provider: ProviderGrok, WorkDir: a.startCfg.WorkDir, SessionID: "grok-dest-1", Model: "grok-4"}
		return reg.registerLocked(next)
	}

	got, err := reg.Migrate(context.Background(), "seat", MigrateArgs{Provider: ProviderGrok, Model: "grok-4"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if migrateCalls != 1 {
		t.Fatalf("migrateLive calls = %d, want 1", migrateCalls)
	}
	if !got.Live {
		t.Fatal("want Live=true for a seat with a live handle")
	}
	if got.Source.Provider != ProviderClaude || got.Destination.Provider != ProviderGrok {
		t.Fatalf("got = %+v", got)
	}
	if got.Agent != proc {
		t.Fatal("want the same live handle returned")
	}
}

// TestRegistryMigrateLiveSeatTurnInFlightRefusesWithoutForce covers the
// turn-in-flight failure mode: a live seat mid-response refuses the move
// unless the caller forces it.
func TestRegistryMigrateLiveSeatTurnInFlightRefusesWithoutForce(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		return startWithBackend(cfg, &fakeAgentBackend{name: "fake-claude"})
	}
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "seat", Provider: ProviderClaude, WorkDir: t.TempDir(), SessionID: "src"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)

	var calls int
	prevLive := migrateLive
	t.Cleanup(func() { migrateLive = prevLive })
	migrateLive = func(a *Agent, args *MigrateArgs) error {
		calls++
		return errors.New("Migrate: turn in flight; wait for the current response or Interrupt first")
	}

	_, err = reg.Migrate(context.Background(), "seat", MigrateArgs{Provider: ProviderGrok}, "")
	if err == nil || !strings.Contains(err.Error(), "turn in flight") {
		t.Fatalf("err = %v, want turn-in-flight refusal", err)
	}
	if calls != 1 {
		t.Fatalf("migrateLive calls = %d, want exactly 1 (no retry without force)", calls)
	}
	if def := reg.Def("seat"); def == nil || def.Provider != ProviderClaude {
		t.Fatalf("seat moved despite refusal: %+v", def)
	}
}

// TestRegistryMigrateLiveSeatForceInterruptsThenRetries is the
// force-recovery failure mode named in the brief: a forced migrate that
// finds a turn in flight interrupts it and retries once, rather than
// leaving the host to decide that outside Claudia.
func TestRegistryMigrateLiveSeatForceInterruptsThenRetries(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	prevSettle := migrateInterruptSettle
	migrateInterruptSettle = 0
	t.Cleanup(func() { migrateInterruptSettle = prevSettle })

	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	backend := &fakeAgentBackend{name: "fake-claude"}
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		return startWithBackend(cfg, backend)
	}
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "seat", Provider: ProviderClaude, WorkDir: t.TempDir(), SessionID: "src"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)

	var calls int
	prevLive := migrateLive
	t.Cleanup(func() { migrateLive = prevLive })
	migrateLive = func(a *Agent, args *MigrateArgs) error {
		calls++
		if calls == 1 {
			return errors.New("Migrate: turn in flight; wait for the current response or Interrupt first")
		}
		return reg.registerLocked(AgentDef{Name: "seat", Provider: ProviderGrok, WorkDir: a.startCfg.WorkDir, SessionID: "grok-dest-1"})
	}

	got, err := reg.Migrate(context.Background(), "seat", MigrateArgs{Provider: ProviderGrok, Force: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("migrateLive calls = %d, want interrupt-then-retry (2)", calls)
	}
	backend.mu.Lock()
	interrupts := backend.interrupts
	backend.mu.Unlock()
	if interrupts != 1 {
		t.Fatalf("backend interrupts = %d, want 1", interrupts)
	}
	if got.Destination.Provider != ProviderGrok {
		t.Fatalf("got = %+v", got)
	}
}

// TestRegistryMigrateAlreadyOnProviderRefuses mirrors the existing
// same-provider refusal, now reached through the single entry point.
func TestRegistryMigrateAlreadyOnProviderRefuses(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "seat", Provider: ProviderClaude, WorkDir: t.TempDir(), SessionID: "src"}); err != nil {
		t.Fatal(err)
	}
	_, err = reg.Migrate(context.Background(), "seat", MigrateArgs{Provider: ProviderClaude}, "")
	if err == nil || !strings.Contains(err.Error(), "already on") {
		t.Fatalf("err = %v, want already-on-provider refusal", err)
	}
}

// TestRegistryMigrateNoSuchSeat covers the missing-seat failure mode.
func TestRegistryMigrateNoSuchSeat(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.Migrate(context.Background(), "ghost", MigrateArgs{Provider: ProviderGrok}, "")
	if err == nil || !strings.Contains(err.Error(), "no such seat") {
		t.Fatalf("err = %v, want no-such-seat", err)
	}
}

// TestRegistryMigrateStoppedSeatSuccess covers the stopped-seat branch of
// the single entry point: no live handle, so the full
// transfer-then-launch sequence runs inside MigrateStopped, reached only
// through Registry.Migrate — the analogue of the brief's "normal
// migration success" case for a seat with nothing live behind it.
func TestRegistryMigrateStoppedSeatSuccess(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name: "seat", Provider: ProviderGrok, SessionID: "source", Goal: "finish the mission", WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	reg.migrationSummarizer = func(_ context.Context, args MigrationTransferArgs) (MigrationTransferResult, error) {
		return MigrationTransferResult{Brief: "Continue the violet mission."}, nil
	}
	destinationBackend := &fakeAgentBackend{name: "destination"}
	reg.SetLaunchers(&RegistryLaunchers{
		Adopt: func(Config) (*Agent, error) { return nil, ErrNoSessionWindow },
		Start: func(_ context.Context, cfg Config) (*Agent, error) {
			if cfg.AdoptOnly {
				return nil, ErrNoSessionWindow
			}
			return startWithBackend(cfg, destinationBackend)
		},
	})

	got, err := reg.Migrate(context.Background(), "seat", MigrateArgs{Provider: ProviderClaude}, "user: remember violet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if got.Agent != nil {
			got.Agent.Stop()
		}
	})
	if got.Live {
		t.Fatal("want Live=false for a seat with no live handle")
	}
	if got.Source.Provider != ProviderGrok || PlanProvider(got.Destination.Provider) != ProviderClaude {
		t.Fatalf("got = %+v", got)
	}
	if got.Transfer.Brief != "Continue the violet mission." {
		t.Fatalf("transfer = %+v", got.Transfer)
	}
	destinationBackend.mu.Lock()
	sends := append([]string(nil), destinationBackend.sends...)
	destinationBackend.mu.Unlock()
	if len(sends) != 1 || !strings.Contains(sends[0], "violet") {
		t.Fatalf("destination sends = %v", sends)
	}
}

// TestRegistryMigrateStoppedSeatNoContextRefusesWithoutForce is the
// brief's "whatever other failure modes the original jevons code
// handled" case for the stopped path: an empty transcript with no
// retained history refuses rather than minting a blank destination,
// exactly as MigrateStopped does directly, now reached only through the
// single entry point.
func TestRegistryMigrateStoppedSeatNoContextRefusesWithoutForce(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "seat", Provider: ProviderGrok, SessionID: "source", WorkDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	_, err = reg.Migrate(context.Background(), "seat", MigrateArgs{Provider: ProviderClaude}, "")
	if err == nil || !strings.Contains(err.Error(), "no predecessor context") {
		t.Fatalf("err = %v, want no-predecessor-context refusal", err)
	}
}

// TestRegistryMigratePendingSeedRetriesWithoutRepayingForTransfer is the
// retry-across-failed-launch failure mode (🎯T1013.5's named risk,
// analogous to "a seed that never arrived"): a destination persisted by
// a prior attempt whose launch failed must retry through the single
// entry point without paying for a second context-transfer summary and
// without the caller orchestrating anything extra — exactly
// registry_migrate_stopped_test.go's own scenario, now reached only
// through Registry.Migrate both times.
func TestRegistryMigratePendingSeedRetriesWithoutRepayingForTransfer(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name: "worker", Provider: ProviderGrok, SessionID: "source", Goal: "finish the mission", WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	summaries := 0
	reg.migrationSummarizer = func(_ context.Context, args MigrationTransferArgs) (MigrationTransferResult, error) {
		summaries++
		return MigrationTransferResult{Brief: "Continue the violet mission."}, nil
	}
	starts := 0
	destinationBackend := &fakeAgentBackend{name: "destination"}
	reg.SetLaunchers(&RegistryLaunchers{
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
	first, err := reg.Migrate(context.Background(), "worker", args, "user: remember violet")
	if err == nil || !strings.Contains(err.Error(), "launch failed") {
		t.Fatalf("first attempt = %+v, %v; want persisted launch failure", first, err)
	}
	if summaries != 1 || PlanProvider(first.Destination.Provider) != ProviderClaude || first.Destination.SessionID == "" {
		t.Fatalf("first attempt lost the prepared destination: %+v summaries=%d", first, summaries)
	}
	if first.Live {
		t.Fatal("want Live=false: no live handle existed for this seat")
	}

	second, err := reg.Migrate(context.Background(), "worker", args, "a different transcript must not be summarized")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Agent.Stop)
	if summaries != 1 {
		t.Fatalf("retry re-paid for context transfer: summaries=%d", summaries)
	}
	if starts != 2 {
		t.Fatalf("retry did not resume the one persisted destination: starts=%d", starts)
	}
	if second.Destination.SessionID != first.Destination.SessionID {
		t.Fatalf("retry minted a different destination: first=%q second=%q", first.Destination.SessionID, second.Destination.SessionID)
	}
	destinationBackend.mu.Lock()
	sends := append([]string(nil), destinationBackend.sends...)
	destinationBackend.mu.Unlock()
	if len(sends) != 1 || !strings.Contains(sends[0], "violet") {
		t.Fatalf("retry did not deliver the one prepared brief exactly once: %v", sends)
	}
}

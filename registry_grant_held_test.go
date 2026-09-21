// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

func registryWithAdoptError(t *testing.T, adoptErr error) (*Registry, *int) {
	t.Helper()
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	reg.SetLaunchers(&RegistryLaunchers{
		Start: func(ctx context.Context, cfg Config) (*Agent, error) {
			starts++
			return StartStub(ctx, cfg, nil)
		},
		Adopt: func(Config) (*Agent, error) { return nil, adoptErr },
	})
	if err := reg.Register(AgentDef{
		Name: "jevons", WorkDir: t.TempDir(), SessionID: "d3f2f3b7-8687-42ae-97c7-620faaf812f7",
		Provider: ProviderClaude, TermLogPath: "-",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.StopAll)
	return reg, &starts
}

// A jevons overseer ran as two Claude processes on one session on
// 2026-09-22: the host's second adopt was refused with grant_held — its own
// earlier connection held the seat — and AdoptOrLaunch answered that refusal
// by launching. The owner's messages went to the process nobody was reading.
func TestAdoptOrLaunchDoesNotLaunchOverASeatTheDaemonHolds(t *testing.T) {
	held := fmt.Errorf("broker protocol: %w", &broker.ProtocolError{
		Code: broker.CodeGrantHeld, Field: "name", Value: "jevons",
		Msg: "grant jevons is owned by another connection",
	})
	reg, starts := registryWithAdoptError(t, held)

	a, err := reg.AdoptOrLaunch("jevons")
	if err == nil || a != nil {
		t.Fatalf("a held seat was handed out: agent=%v err=%v", a, err)
	}
	if !grantHeldElsewhere(err) {
		t.Fatalf("the refusal lost its cause: %v", err)
	}
	if *starts != 0 {
		t.Fatalf("launched %d process(es) over a seat the daemon holds live", *starts)
	}
}

// The control: an adopt that fails for any other reason still falls back, so
// a seat whose window is simply gone comes back.
func TestAdoptOrLaunchStillFallsBackOnOtherAdoptErrors(t *testing.T) {
	reg, starts := registryWithAdoptError(t, errors.New("acp initialize: context deadline exceeded"))

	a, err := reg.AdoptOrLaunch("jevons")
	if err != nil || a == nil {
		t.Fatalf("fallback launch refused: %v", err)
	}
	if *starts != 1 {
		t.Fatalf("starts = %d, want 1", *starts)
	}
}

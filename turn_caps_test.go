// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
)

// The turn-caps table is total over the provider set and matches the
// design's backend mapping (docs/design/steer-interrupt-turn-api.md).
// A provider added without a row here reads as nothing-supported, which
// is fail-closed but wrong; this test makes the omission loud.
func TestProviderTurnCapsIsTotal(t *testing.T) {
	t.Parallel()
	want := map[Provider]TurnCaps{
		ProviderClaude:  {CanInterrupt: true, CanSteer: false, SteerPolicy: SteerQueueUntilIdle, BusyOnSecondSubmit: BusySubmitQueue},
		ProviderCursor:  {CanInterrupt: true, CanSteer: true, SteerPolicy: SteerBreakpoint, BusyOnSecondSubmit: BusySubmitReject},
		ProviderGrok:    {CanInterrupt: true, CanSteer: true, SteerPolicy: SteerFinishSlice, BusyOnSecondSubmit: BusySubmitReject},
		ProviderCodex:   {CanInterrupt: true, CanSteer: true, SteerPolicy: SteerFinishSlice, BusyOnSecondSubmit: BusySubmitReject},
		ProviderBedrock: {SteerPolicy: SteerNone},
		ProviderOllama:  {SteerPolicy: SteerNone},
	}
	providers := []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderBedrock, ProviderOllama, ProviderCursor}
	for _, provider := range providers {
		if _, ok := providerTurnCaps[provider]; !ok {
			t.Errorf("provider %q has no turn-caps row", provider)
			continue
		}
		if got := ProviderTurnCaps(provider); got != want[provider] {
			t.Errorf("ProviderTurnCaps(%s) = %+v, want %+v", provider, got, want[provider])
		}
	}
	for provider := range providerTurnCaps {
		found := false
		for _, p := range providers {
			found = found || p == provider
		}
		if !found {
			t.Errorf("turn-caps row for %q names a provider outside the set", provider)
		}
	}
	if got := ProviderTurnCaps(""); got != want[ProviderClaude] {
		t.Errorf("ProviderTurnCaps(\"\") = %+v, want Claude", got)
	}
	if got := ProviderTurnCaps(Provider("nope")); got.CanInterrupt || got.CanSteer || got.SteerPolicy != SteerNone {
		t.Errorf("unknown provider = %+v, want nothing supported", got)
	}
}

// Withdrawing the steer claim relabels the policy honestly and leaves a
// none-policy alone.
func TestTurnCapsWithoutSteer(t *testing.T) {
	t.Parallel()
	got := ProviderTurnCaps(ProviderCursor).withoutSteer()
	if got.CanSteer || got.SteerPolicy != SteerQueueUntilIdle || !got.CanInterrupt || got.BusyOnSecondSubmit != BusySubmitReject {
		t.Errorf("Cursor without steer = %+v", got)
	}
	if got := ProviderTurnCaps(ProviderOllama).withoutSteer(); got.SteerPolicy != SteerNone {
		t.Errorf("Ollama without steer = %+v, want policy none", got)
	}
}

// A live handle's TurnCaps is the contract narrowed to what is wired:
// steer only when a steer hook exists, interrupt only when one exists,
// and a backend refinement (turnCaps hook) takes precedence over the
// static row.
func TestAgentTurnCapsReflectsWiring(t *testing.T) {
	t.Parallel()
	unwired := NewStubAgentOps(&StubAgentOps{Provider: ProviderGrok})
	if caps := unwired.TurnCaps(); caps.CanSteer || caps.CanInterrupt || caps.SteerPolicy != SteerQueueUntilIdle {
		t.Errorf("unwired Grok = %+v", caps)
	}
	wired := NewStubAgentOps(&StubAgentOps{
		Provider:  ProviderGrok,
		Steer:     func(string) (DeliveryOutcome, error) { return DeliveryOutcome{}, nil },
		Interrupt: func() error { return nil },
	})
	if caps := wired.TurnCaps(); caps != ProviderTurnCaps(ProviderGrok) {
		t.Errorf("wired Grok = %+v, want contract %+v", caps, ProviderTurnCaps(ProviderGrok))
	}
	refined := NewStubAgentOps(&StubAgentOps{
		Provider:  ProviderCodex,
		Steer:     func(string) (DeliveryOutcome, error) { return DeliveryOutcome{}, nil },
		Interrupt: func() error { return nil },
		TurnCaps:  func() TurnCaps { return codexTurnCaps(false) },
	})
	if caps := refined.TurnCaps(); caps.CanSteer || caps.SteerPolicy != SteerQueueUntilIdle || !caps.CanInterrupt {
		t.Errorf("refined Codex = %+v, want steer withdrawn by the backend", caps)
	}
}

// steerOp yields a hook only for a client that satisfies the seam; a
// client without the methods is a nil hook, not a panic.
func TestSteerOpSeam(t *testing.T) {
	t.Parallel()
	if steerOp(struct{}{}) != nil {
		t.Error("steerOp on a non-steerer returned a hook")
	}
	if steerOp(nil) != nil {
		t.Error("steerOp(nil) returned a hook")
	}
	var typedNil *codexAppServerClient
	if steerOp(typedNil) == nil {
		t.Error("steerOp on a typed nil steerer returned no hook; the interface is satisfied even when the pointer is nil")
	}
	hook := steerOp(fakeSteerer{mechanism: "m", superseded: "t9"})
	if hook == nil {
		t.Fatal("steerOp on a steerer returned nil")
	}
	out, err := hook(nil, "text")
	if err != nil || out.Mechanism != "m" || out.SupersededTurnID != "t9" {
		t.Errorf("hook = %+v / %v", out, err)
	}
}

type fakeSteerer struct {
	mechanism, superseded string
}

func (f fakeSteerer) Steer(_ context.Context, _ string) (string, error) { return f.mechanism, nil }
func (f fakeSteerer) SupersededTurnID() string                          { return f.superseded }

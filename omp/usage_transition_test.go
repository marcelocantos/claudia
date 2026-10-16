// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"reflect"
	"testing"
)

func usageTransitionPair() (UsageFact, UsageFact) {
	start := UsageFact{Seq: 1, CallID: "physical", ProviderAttemptID: "physical", InvocationID: "invocation", Seat: "seat", SessionID: "session", Kind: "model", State: "started", Provider: "cursor", Model: "model", TurnID: "turn", Cause: "owner", Requests: []UsageRequestRef{{RequestID: "logical", AttemptID: "delivery"}}, StartedAt: "2026-10-16T01:00:00Z", CounterSource: "provider_reported", Coverage: "provider_attempt"}
	final := start
	final.Seq, final.State, final.FinishedAt = 3, "completed", "2026-10-16T01:00:01Z"
	final.ProviderRequestID, final.ProviderResponseID = "wire-request", "wire-response"
	context, protocol, total, zero, cost := int64(23), int64(7), int64(12), int64(0), 0.1
	final.ContextTokens, final.ProtocolTokenDeltaTotal = &context, &protocol
	final.Counters = UsageCounters{TotalInputTokens: &total, OutputTokens: &zero}
	final.APIEquivalentCostUSD, final.CostSource = &cost, "provider_adapter_estimate"
	return start, final
}

func TestUsageTransitionApplyAndReplay(t *testing.T) {
	start, final := usageTransitionPair()
	for _, tc := range []struct {
		name               string
		previous, incoming *UsageFact
		apply              bool
	}{
		{"first start", nil, &start, true},
		{"first terminal", nil, &final, true},
		{"terminal measurements and IDs", &start, &final, true},
		{"duplicate start", &start, &start, false},
		{"duplicate final", &final, &final, false},
		{"start after final replay", &final, &start, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeStart, beforeFinal := start, final
			apply, err := ValidateUsageTransition(tc.previous, tc.incoming)
			if err != nil || apply != tc.apply {
				t.Fatalf("apply=%v err=%v", apply, err)
			}
			if !reflect.DeepEqual(start, beforeStart) || !reflect.DeepEqual(final, beforeFinal) {
				t.Fatal("validation mutated facts")
			}
		})
	}
	for _, state := range []string{"completed", "failed", "cancelled", "unknown"} {
		f := final
		f.State = state
		if apply, err := ValidateUsageTransition(&start, &f); err != nil || !apply {
			t.Fatalf("terminal %s: %v %v", state, apply, err)
		}
	}
}

func TestUsageTransitionRejectsInvalidFactsAndTransitions(t *testing.T) {
	for name, mutate := range map[string]func(*UsageFact, *UsageFact){
		"invalid previous":           func(a, b *UsageFact) { a.Seq = 0 },
		"invalid incoming":           func(a, b *UsageFact) { b.State = "invented" },
		"different seat":             func(a, b *UsageFact) { b.Seat = "foreign" },
		"different session":          func(a, b *UsageFact) { b.SessionID = "foreign" },
		"different call":             func(a, b *UsageFact) { b.CallID, b.ProviderAttemptID = "foreign", "foreign" },
		"different invocation":       func(a, b *UsageFact) { b.InvocationID = "foreign" },
		"different provider":         func(a, b *UsageFact) { b.Provider = "foreign" },
		"different model":            func(a, b *UsageFact) { b.Model = "foreign" },
		"different kind":             func(a, b *UsageFact) { b.Kind = "compaction" },
		"different turn":             func(a, b *UsageFact) { b.TurnID = "foreign" },
		"different cause":            func(a, b *UsageFact) { b.Cause = "foreign" },
		"different started time":     func(a, b *UsageFact) { b.StartedAt = "2026-10-16T01:00:00.001Z" },
		"different input references": func(a, b *UsageFact) { b.Requests = []UsageRequestRef{{RequestID: "logical", AttemptID: "foreign"}} },
		"changed scope": func(a, b *UsageFact) {
			b.Coverage, b.CounterSource, b.InvocationID, b.ProviderAttemptID = "engine_invocation", "adapter_normalized_positive", "", ""
		},
		"same sequence new terminal": func(a, b *UsageFact) { b.Seq = a.Seq },
		"newer start":                func(a, b *UsageFact) { *b = *a; b.Seq++ },
		"older terminal":             func(a, b *UsageFact) { a.Seq = b.Seq + 1 },
		"negative protocol":          func(a, b *UsageFact) { n := int64(-1); b.ProtocolTokenDeltaTotal = &n },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := usageTransitionPair()
			mutate(&a, &b)
			if apply, err := ValidateUsageTransition(&a, &b); err == nil || apply {
				t.Fatalf("invalid transition apply=%v err=%v", apply, err)
			}
		})
	}
	if apply, err := ValidateUsageTransition(nil, nil); err == nil || apply {
		t.Fatal("nil incoming accepted")
	}
}

func TestUsageTransitionTerminalFactsCannotBeRewritten(t *testing.T) {
	_, terminal := usageTransitionPair()
	for name, change := range map[string]func(*UsageFact){
		"newer final":           func(f *UsageFact) { f.Seq++ },
		"older final":           func(f *UsageFact) { f.Seq-- },
		"different status":      func(f *UsageFact) { f.State = "failed" },
		"different request ID":  func(f *UsageFact) { f.ProviderRequestID = "changed" },
		"different response ID": func(f *UsageFact) { f.ProviderResponseID = "changed" },
		"protocol absent":       func(f *UsageFact) { f.ProtocolTokenDeltaTotal = nil },
		"context absent":        func(f *UsageFact) { f.ContextTokens = nil },
		"zero becomes unknown":  func(f *UsageFact) { f.Counters.OutputTokens = nil },
	} {
		t.Run(name, func(t *testing.T) {
			incoming := terminal
			change(&incoming)
			if apply, err := ValidateUsageTransition(&terminal, &incoming); err == nil || apply {
				t.Fatalf("rewrote terminal: %v %v", apply, err)
			}
		})
	}
	start, _ := usageTransitionPair()
	start.InvocationID = "foreign"
	if apply, err := ValidateUsageTransition(&terminal, &start); err == nil || apply {
		t.Fatal("older start bypassed identity check")
	}
}

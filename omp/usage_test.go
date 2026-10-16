// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"encoding/json"
	"math"
	"testing"
)

func TestUsageAvailabilityAndBoundaryValidation(t *testing.T) {
	zero := int64(0)
	f := UsageFact{Seq: 2, CallID: "call", Seat: "seat", SessionID: "session", Kind: "model", State: "completed", Provider: "mock", Model: "mock", Requests: []UsageRequestRef{}, StartedAt: "2026-10-16T01:00:00Z", FinishedAt: "2026-10-16T01:00:01Z", Counters: UsageCounters{OutputTokens: &zero}, CounterSource: "adapter_normalized_positive", Coverage: "engine_invocation"}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var decoded UsageFact
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Counters.InputTokens != nil || decoded.Counters.OutputTokens == nil || *decoded.Counters.OutputTokens != 0 {
		t.Fatal("unknown and known zero collapsed")
	}
	if err := decoded.ValidateIdentity("seat", "session"); err != nil {
		t.Fatal(err)
	}
	for label, mutate := range map[string]func(*UsageFact){
		"foreign session": func(f *UsageFact) { f.SessionID = "other" },
		"negative":        func(f *UsageFact) { n := int64(-1); f.Counters.InputTokens = &n },
		"unsafe integer":  func(f *UsageFact) { n := int64(1 << 53); f.Counters.InputTokens = &n },
		"nonfinite cost": func(f *UsageFact) {
			n := math.Inf(1)
			f.APIEquivalentCostUSD = &n
			f.CostSource = "provider_adapter_estimate"
		},
		"unknown source":      func(f *UsageFact) { f.CounterSource = "full_snapshot" },
		"unfinished measured": func(f *UsageFact) { f.State = "started" },
		"duplicated reference": func(f *UsageFact) {
			f.Requests = []UsageRequestRef{{RequestID: "a", AttemptID: "1"}, {RequestID: "a", AttemptID: "2"}}
		},
	} {
		t.Run(label, func(t *testing.T) {
			bad := f
			mutate(&bad)
			if bad.ValidateIdentity("seat", "session") == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	p := UsagePage{Seat: "seat", SessionID: "session", Facts: []UsageFact{f}, Next: 2}
	if err := p.Validate("seat", "session", 1, 1); err != nil {
		t.Fatal(err)
	}
	if p.Validate("seat", "session", 0, 1) == nil {
		t.Fatal("cursor gap accepted")
	}
	p.Facts = []UsageFact{}
	p.Next = 1
	p.More = true
	if p.Validate("seat", "session", 1, 1) == nil {
		t.Fatal("nonadvancing pagination accepted")
	}
}

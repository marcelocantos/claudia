// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// This reaches the socket contract, not a message-construction helper.
func TestQueryUsageForwardsDeadline(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "usage-deadline-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "sidecar.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv(SocketEnv, socket)
	deadline := time.Now().Add(3 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	messages := make(chan Message, 1)
	failures := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			failures <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(deadline)
		var msg Message
		if err := json.NewDecoder(conn).Decode(&msg); err != nil {
			failures <- err
			return
		}
		messages <- msg
		failures <- json.NewEncoder(conn).Encode(Event{Type: "usage_page", RPCID: msg.RPCID, UsagePage: &UsagePage{Seat: "seat", SessionID: "session", Facts: []UsageFact{}, Next: 0}})
	}()
	page, err := QueryUsage(ctx, "seat", "session", 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	if page.More || len(page.Facts) != 0 {
		t.Fatalf("bad page: %+v", page)
	}
	select {
	case msg := <-messages:
		if msg.ExpiresAt != deadline.UnixMilli() {
			t.Fatalf("deadline=%d, want %d", msg.ExpiresAt, deadline.UnixMilli())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
}

func TestUsageProviderAttemptIdentityAndInclusiveInput(t *testing.T) {
	total, zero := int64(12), int64(0)
	f := UsageFact{Seq: 2, CallID: "physical-A", ProviderAttemptID: "physical-A", InvocationID: "engine-A", Seat: "seat", SessionID: "session", Kind: "model", State: "failed", Provider: "xai-oauth", Model: "grok", Requests: []UsageRequestRef{{RequestID: "host-A", AttemptID: "delivery-A"}}, StartedAt: "2026-10-16T01:00:00Z", FinishedAt: "2026-10-16T01:00:01Z", Counters: UsageCounters{TotalInputTokens: &total, OutputTokens: &zero}, CounterSource: "provider_reported", Coverage: "provider_attempt"}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var decoded UsageFact
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateIdentity("seat", "session"); err != nil {
		t.Fatal(err)
	}
	if decoded.Counters.TotalInputTokens == nil || *decoded.Counters.TotalInputTokens != 12 || decoded.Counters.InputTokens != nil || decoded.Counters.OutputTokens == nil || *decoded.Counters.OutputTokens != 0 {
		t.Fatal("inclusive input, unknown fresh input or explicit output zero lost")
	}
	for label, mutate := range map[string]func(*UsageFact){
		"missing invocation":          func(f *UsageFact) { f.InvocationID = "" },
		"different physical identity": func(f *UsageFact) { f.ProviderAttemptID = "other" },
		"synthetic physical evidence": func(f *UsageFact) { f.CounterSource = "adapter_normalized_positive" },
		"physical IDs on aggregate":   func(f *UsageFact) { f.Coverage = "engine_invocation"; f.CounterSource = "adapter_normalized_positive" },
	} {
		t.Run(label, func(t *testing.T) {
			bad := f
			mutate(&bad)
			if bad.ValidateIdentity("seat", "session") == nil {
				t.Fatal("invalid physical observation accepted")
			}
		})
	}
}

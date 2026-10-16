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
		"foreign session":   func(f *UsageFact) { f.SessionID = "other" },
		"negative":          func(f *UsageFact) { n := int64(-1); f.Counters.InputTokens = &n },
		"negative protocol": func(f *UsageFact) { n := int64(-1); f.ProtocolTokenDeltaTotal = &n },
		"unsafe integer":    func(f *UsageFact) { n := int64(1 << 53); f.Counters.InputTokens = &n },
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
// 🎯T97 exemption: an expired attempt without a captured request is retried,
// never scored. Only captured wire data decides exact deadline forwarding;
// go test -timeout bounds a fixture that never responds.
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
	messages := make(chan Message, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // The test closes the listener on completion or failure.
			}
			var msg Message
			if err := json.NewDecoder(conn).Decode(&msg); err != nil {
				conn.Close()
				continue // A caller can expire before it sends the request.
			}
			messages <- msg
			// A caller can expire after its request was captured. Its wire
			// deadline still supplies evidence; the reply need not arrive.
			_ = json.NewEncoder(conn).Encode(Event{Type: "usage_page", RPCID: msg.RPCID, UsagePage: &UsagePage{Seat: "seat", SessionID: "session", Facts: []UsageFact{}, Next: 0}})
			conn.Close()
			return
		}
	}()
	issued := map[int64]bool{}
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		deadline, _ := ctx.Deadline()
		issued[deadline.UnixMilli()] = true
		page, err := QueryUsage(ctx, "seat", "session", 0, 128)
		expired := ctx.Err() != nil
		cancel()
		if err != nil && !expired {
			t.Fatal(err)
		}
		if err == nil && (page.More || len(page.Facts) != 0) {
			t.Fatalf("bad page: %+v", page)
		}
		select {
		case msg := <-messages:
			if !issued[msg.ExpiresAt] {
				t.Fatalf("sidecar expiry %d was not an original caller expiry: %v", msg.ExpiresAt, issued)
			}
			return
		default:
			if !expired {
				t.Fatal("successful query produced no captured request")
			}
		}
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

func TestUsageProtocolDeltaAvailability(t *testing.T) {
	for _, raw := range []string{`{"protocol_token_delta_total":0}`, `{}`} {
		var fact UsageFact
		if err := json.Unmarshal([]byte(raw), &fact); err != nil {
			t.Fatal(err)
		}
		if (fact.ProtocolTokenDeltaTotal == nil) != (raw == `{}`) {
			t.Fatal("protocol omitted and zero values collapsed")
		}
	}
}

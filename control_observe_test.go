// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

func TestSeatPumpRecordsUsageAndReceipt(t *testing.T) {
	var usage *UsageFact
	var receipts []*RequestReceipt
	SetControlPlaneObservers(
		func(fact *UsageFact) error {
			copied := *fact
			usage = &copied
			return nil
		},
		func(receipt *RequestReceipt) error {
			copied := *receipt
			receipts = append(receipts, &copied)
			return nil
		},
	)
	t.Cleanup(func() { SetControlPlaneObservers(nil, nil) })

	_, a, _, peer, events := receiptPipe(t)
	a.sessionID = "generation"
	fact := UsageFact{
		Seq: 1, CallID: "call-1", Seat: "seat", SessionID: "generation",
		Kind: "model", State: "started", Provider: "openai", Model: "gpt-5.5",
		Requests: []UsageRequestRef{}, StartedAt: "2026-10-16T01:00:00Z",
		CounterSource: "adapter_normalized_positive", Coverage: "engine_invocation",
	}
	replyReceipt(t, peer, omp.Event{Type: "usage", Seat: "seat", UsageFact: &fact})
	ev := <-events
	if ev.Type != "usage" || ev.UsageFact == nil || ev.UsageFact.CallID != "call-1" {
		t.Fatalf("pump dropped usage event: %+v", ev)
	}
	if usage == nil || usage.Seq != 1 || usage.CallID != "call-1" || usage.SessionID != "generation" {
		t.Fatalf("pump did not record usage: %+v", usage)
	}

	receipt := testReceipt("logical-A")
	replyReceipt(t, peer, omp.Event{Type: "receipt", Seat: "seat", RequestID: "logical-A", Receipt: receipt})
	ev = <-events
	if ev.Type != "receipt" || ev.Receipt == nil || ev.Receipt.RequestID != "logical-A" {
		t.Fatalf("pump dropped receipt event: %+v", ev)
	}
	if len(receipts) != 1 || receipts[0].RequestID != "logical-A" || receipts[0].State != ReceiptAccepted {
		t.Fatalf("pump did not record receipt: %+v", receipts)
	}

	replyReceipt(t, peer, omp.Event{Type: "accepted", Seat: "seat", RequestID: "logical-A", Receipt: receipt})
	ev = <-events
	if ev.Type != "progress" || len(receipts) != 2 || receipts[1].RequestID != "logical-A" {
		t.Fatalf("acceptance did not record the receipt: event=%+v receipts=%d", ev, len(receipts))
	}
}

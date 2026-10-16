// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

func testReceipt(request string) *RequestReceipt {
	return &RequestReceipt{Seat: "seat", SessionID: "generation", RequestID: request, AttemptID: "original-attempt", State: ReceiptAccepted, TurnIDs: []string{}, AcceptedAt: "2026-10-16T01:00:00Z", UpdatedAt: "2026-10-16T01:00:00Z"}
}

func receiptPipe(t *testing.T) (*ompControl, *Agent, *bufio.Reader, net.Conn, <-chan Event) {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("cl-receipt-%d-%d.sock", os.Getpid(), ompFixtureSeq.Add(1)))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := omp.Dial(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	ctrl := &ompControl{conn: conn, bytes: make(chan []byte, 32), seat: "seat", sessionID: "generation"}
	a := &Agent{alive: true, ready: closedReady(), eventSubs: map[int64]EventFunc{}}
	a.ops = agentOps{explicitRequestIDs: true, send: func(*Agent, string) error { return nil }, sendWithRequestID: func(a *Agent, text, id string) error {
		return ctrl.sendRequest(promptMessage(omp.OpPrompt, "seat", text, a, ctrl), id)
	}, receipt: func(ctx context.Context, _ *Agent, id string) (*RequestReceipt, error) { return ctrl.receipt(ctx, id) }}
	events := make(chan Event, 32)
	a.eventSubs[1] = func(ev Event) { events <- ev }
	finished := make(chan struct{})
	go func() { ctrl.pump(a); close(finished) }()
	t.Cleanup(func() { _ = conn.Close(); _ = peer.Close(); <-finished; _ = os.Remove(path) })
	return ctrl, a, bufio.NewReader(peer), peer, events
}

func nextReceiptMessage(t *testing.T, r *bufio.Reader) omp.Message {
	t.Helper()
	raw, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var msg omp.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func replyReceipt(t *testing.T, c net.Conn, ev omp.Event) {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestIdentifiedSidecarSendWaitsForMatchingDurableReceipt(t *testing.T) {
	for _, op := range []string{omp.OpPrompt, omp.OpSteer} {
		t.Run(op, func(t *testing.T) {
			ctrl, a, r, peer, events := receiptPipe(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- ctrl.sendIdentified(ctx, omp.Message{Op: op, Seat: "seat", Text: "B"}, "B") }()
			msg := nextReceiptMessage(t, r)
			if msg.RequestID != "B" || msg.SessionID != "generation" || msg.AttemptID == "" || msg.RPCID == "" || msg.RPCID == msg.RequestID || msg.AttemptID == msg.RPCID {
				t.Fatalf("identities conflated: %+v", msg)
			}
			// A ends while B is queued. Its terminal cannot acknowledge B.
			replyReceipt(t, peer, omp.Event{Type: "turn_end", Seat: "seat", TurnID: "turn-A", RequestID: "A"})
			if ev := <-events; ev.RequestID != "A" {
				t.Fatalf("explicit A overwritten: %+v", ev)
			}
			select {
			case err := <-done:
				t.Fatalf("unrelated turn settled B: %v", err)
			default:
			}
			wrong := testReceipt("A")
			replyReceipt(t, peer, omp.Event{Type: "receipt", Seat: "seat", RequestID: "A", Receipt: wrong})
			<-events
			select {
			case err := <-done:
				t.Fatalf("unrelated receipt settled B: %v", err)
			default:
			}
			replyReceipt(t, peer, omp.Event{Type: "accepted", Seat: "seat", RPCID: msg.RPCID, RequestID: "B", Receipt: testReceipt("B")})
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if ev := <-events; ev.Receipt == nil || ev.RequestID != "B" {
				t.Fatalf("missing typed receipt: %+v", ev)
			}
			a.mu.Lock()
			pending := len(a.requestPending)
			a.mu.Unlock()
			if pending != 0 {
				t.Fatalf("explicit path leaked FIFO: %d", pending)
			}
		})
	}
}

func TestLostReceiptAcknowledgementQueriesOriginalWithoutReplay(t *testing.T) {
	ctrl, a, r, peer, _ := receiptPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ctrl.sendIdentified(ctx, omp.Message{Op: omp.OpPrompt, Seat: "seat", Text: "B"}, "B") }()
	msg := nextReceiptMessage(t, r)
	// The journal accepted, but the acknowledgment was lost.
	cancel()
	if err := <-done; !errors.Is(err, ErrReceiptUncertain) || !errors.Is(err, context.Canceled) {
		t.Fatalf("lost ACK = %v", err)
	}
	ctrl.mu.Lock()
	pending := len(ctrl.waits)
	ctrl.mu.Unlock()
	if pending != 0 {
		t.Fatalf("RPC waiter leaked: %d", pending)
	}
	type result struct {
		r   *RequestReceipt
		err error
	}
	queried := make(chan result, 1)
	go func() { receipt, err := a.Receipt(context.Background(), "B"); queried <- result{receipt, err} }()
	query := nextReceiptMessage(t, r)
	if query.Op != omp.OpReceipt || query.RequestID != msg.RequestID || query.SessionID != msg.SessionID || query.RPCID == msg.RPCID || query.AttemptID != "" {
		t.Fatalf("query retried or reminted input: %+v", query)
	}
	receipt := testReceipt("B")
	receipt.AttemptID = msg.AttemptID
	replyReceipt(t, peer, omp.Event{Type: "receipt", RPCID: query.RPCID, RequestID: "B", Receipt: receipt})
	got := <-queried
	if got.err != nil || got.r.AttemptID != msg.AttemptID || got.r.State != ReceiptAccepted {
		t.Fatalf("query = %+v, %v", got.r, got.err)
	}
}

func TestSidecarPublicIdentifiedSendAndReceipt(t *testing.T) {
	f := startOMPFixture(t)
	a, err := StartDirectContext(context.Background(), Config{Name: "receipt-seat", SessionID: "receipt-generation", Provider: ProviderGrok, Model: "grok-4.20", WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if err := a.SendWithRequestID("hello", "logical-input"); err != nil {
		t.Fatal(err)
	}
	msg := <-f.prompts
	if msg.RequestID != "logical-input" || msg.AttemptID == "" || msg.SessionID != "receipt-generation" {
		t.Fatalf("public send omitted identity: %+v", msg)
	}
	r, err := a.Receipt(context.Background(), "logical-input")
	if err != nil || r.AttemptID != msg.AttemptID {
		t.Fatalf("receipt: %+v %v", r, err)
	}
	a.mu.Lock()
	pending := len(a.requestPending)
	a.mu.Unlock()
	if pending != 0 {
		t.Fatalf("public send leaked FIFO: %d", pending)
	}
}

func TestReceiptEventBrokerWireRoundTrip(t *testing.T) {
	want := testReceipt("B")
	raw, err := EncodeEventWire(Event{Type: "receipt", RequestID: "B", Receipt: want})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := DecodeEventWire(raw)
	if err != nil || ev.Receipt == nil || ev.Receipt.RequestID != "B" || ev.Receipt.State != ReceiptAccepted {
		t.Fatalf("receipt lost: %+v %v", ev, err)
	}
}

func TestIdentifiedSidecarDoesNotFallBackToLegacyOrForeignAcceptance(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		label := "foreign-receipt"
		if legacy {
			label = "legacy-ack"
		}
		t.Run(label, func(t *testing.T) {
			ctrl, _, r, peer, events := receiptPipe(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- ctrl.sendIdentified(ctx, omp.Message{Op: omp.OpPrompt, Seat: "seat", Text: "B"}, "B") }()
			msg := nextReceiptMessage(t, r)
			if legacy {
				replyReceipt(t, peer, omp.Event{Type: "accepted", Seat: "seat", TurnID: "turn-legacy"})
				<-events // prove the pump handled the old response
				select {
				case err := <-done:
					t.Fatalf("legacy ack was durable evidence: %v", err)
				default:
				}
				cancel()
			} else {
				replyReceipt(t, peer, omp.Event{Type: "accepted", Seat: "seat", RPCID: msg.RPCID, RequestID: "B", Receipt: testReceipt("A")})
			}
			if err := <-done; !errors.Is(err, ErrReceiptUncertain) {
				t.Fatalf("unsafe acceptance = %v", err)
			}
		})
	}
}

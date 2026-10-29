// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

func TestReceiptQueryThroughBrokerWithoutLoadingOrCredentials(t *testing.T) {
	f := newFixture(t)
	socket := filepath.Join(filepath.Dir(f.sock), "receipts.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(omp.SocketEnv, socket)
	seen := make(chan omp.Message, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err != nil {
				_ = conn.Close()
				continue
			}
			var msg omp.Message
			if json.Unmarshal(line, &msg) != nil {
				_ = conn.Close()
				continue
			}
			seen <- msg
			ev := omp.Event{Type: "receipt", RPCID: msg.RPCID, RequestID: msg.RequestID,
				Receipt: &omp.RequestReceipt{Seat: msg.Seat, SessionID: msg.SessionID, RequestID: msg.RequestID, State: omp.ReceiptUnknown, TurnIDs: []string{}, Reason: "not_found"}}
			if msg.RequestID == "corrupt" {
				ev.Type, ev.Text, ev.Receipt = "error", "request journal corrupt", nil
			}
			raw, _ := json.Marshal(ev)
			_, _ = conn.Write(append(raw, '\n'))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	f.boot(t, nil)
	query := claudia.ReceiptQuery{Seat: "unloaded", SessionID: "old-generation", RequestID: "logical-B"}
	// The library's direct seam remains direct with broker routing configured;
	// this is also the daemon's production path, so recursion would deadlock.
	direct, err := claudia.QueryReceiptDirect(context.Background(), query)
	if err != nil || direct.State != claudia.ReceiptUnknown {
		t.Fatalf("direct query with broker configured: %+v %v", direct, err)
	}
	if msg := <-seen; msg.Op != omp.OpReceipt || msg.RequestID != query.RequestID {
		t.Fatalf("direct query changed operation: %+v", msg)
	}
	r, err := claudia.QueryReceipt(context.Background(), query)
	if err != nil || r.State != claudia.ReceiptUnknown || r.Reason != "not_found" {
		t.Fatalf("query: %+v %v", r, err)
	}
	msg := <-seen
	if msg.Op != omp.OpReceipt || msg.RequestID != query.RequestID || msg.SessionID != query.SessionID || msg.Seat != query.Seat || msg.RPCID == "" || msg.RPCID == msg.RequestID || msg.Token != "" || msg.Provider != "" || msg.Model != "" {
		t.Fatalf("query loaded or confused identity: %+v", msg)
	}
	if f.seatCount() != 0 {
		t.Fatal("receipt query launched a seat")
	}
	query.RequestID = "corrupt"
	if r, err := claudia.QueryReceipt(context.Background(), query); r != nil || !errors.Is(err, claudia.ErrReceiptUncertain) {
		t.Fatalf("corrupt store is not uncertainty: %+v %v", r, err)
	}
	<-seen
	// A remote Agent uses the same read RPC; it does not need to reacquire
	// or send a message to learn what happened to its original request.
	a, err := claudia.Start(claudia.Config{Name: "seat", SessionID: "generation", WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if !a.DaemonHeld() {
		t.Fatal("expected broker-owned handle")
	}
	r, err = a.Receipt(context.Background(), "logical-A")
	if err != nil || r.SessionID != "generation" || r.RequestID != "logical-A" {
		t.Fatalf("Agent.Receipt: %+v %v", r, err)
	}
	msg = <-seen
	if msg.Op != omp.OpReceipt || msg.RequestID != "logical-A" {
		t.Fatalf("remote handle replayed: %+v", msg)
	}
}

func TestBrokerIdentifiedSendWaitsForReceiptAndPreservesLostAck(t *testing.T) {
	for _, loseAck := range []bool{false, true} {
		label := "acknowledged"
		if loseAck {
			label = "lost-ack"
		}
		t.Run(label, func(t *testing.T) {
			seedPlans(t) // fixture credentials only; never Keychain or a provider
			f := newFixture(t)
			socket := filepath.Join(filepath.Dir(f.sock), "receipts.sock")
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(omp.SocketEnv, socket)
			prompts := make(chan omp.Message, 1)
			release := make(chan struct{})
			var mu sync.Mutex
			var recorded *omp.RequestReceipt
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer conn.Close()
						r := bufio.NewReader(conn)
						write := func(ev omp.Event) { raw, _ := json.Marshal(ev); _, _ = conn.Write(append(raw, '\n')) }
						for {
							line, err := r.ReadBytes('\n')
							if err != nil {
								return
							}
							var msg omp.Message
							if json.Unmarshal(line, &msg) != nil {
								return
							}
							switch msg.Op {
							case omp.OpLoad, omp.OpAdopt:
								write(omp.Event{Type: "ready", Seat: msg.Seat, How: "launched", Provider: msg.Provider})
							case omp.OpPrompt:
								now := time.Now().UTC().Format(time.RFC3339Nano)
								receipt := &omp.RequestReceipt{Seat: msg.Seat, SessionID: msg.SessionID, RequestID: msg.RequestID, AttemptID: msg.AttemptID, State: omp.ReceiptAccepted, TurnIDs: []string{}, AcceptedAt: now, UpdatedAt: now}
								mu.Lock()
								recorded = receipt
								mu.Unlock()
								prompts <- msg
								write(omp.Event{Type: "turn_end", Seat: msg.Seat, TurnID: "unrelated-turn", RequestID: "A"})
								<-release
								if loseAck {
									return
								}
								write(omp.Event{Type: "accepted", Seat: msg.Seat, RPCID: msg.RPCID, RequestID: msg.RequestID, Receipt: receipt})
							case omp.OpReceipt:
								mu.Lock()
								receipt := recorded
								mu.Unlock()
								write(omp.Event{Type: "receipt", Seat: msg.Seat, RPCID: msg.RPCID, RequestID: msg.RequestID, Receipt: receipt})
							}
						}
					}()
				}
			}()
			t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
			opts := f.options(nil)
			opts.launchers = nil
			opts.DisableResume = true
			opts.DisableMCPHost = true
			f.bootWith(t, opts)
			a, err := claudia.Start(claudia.Config{Name: "receipt-seat", SessionID: "receipt-generation", Provider: claudia.ProviderGrok, Model: "grok-4.20", WorkDir: t.TempDir(), TermLogPath: "-"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Stop)
			if !a.SupportsReceipts() {
				t.Fatal("broker grant omitted durable receipt capability")
			}
			events := make(chan claudia.Event, 8)
			a.SubscribeEvents(func(ev claudia.Event) { events <- ev })
			done := make(chan error, 1)
			go func() { done <- a.SendWithRequestID("B", "logical-B") }()
			msg := <-prompts
			if msg.RequestID != "logical-B" || msg.SessionID != "receipt-generation" || msg.AttemptID == "" || msg.RPCID == "" {
				t.Fatalf("identity lost across broker: %+v", msg)
			}
			for ev := range events {
				if ev.TurnID == "unrelated-turn" {
					if ev.RequestID != "A" {
						t.Fatalf("A misattributed: %+v", ev)
					}
					break
				}
			}
			select {
			case err := <-done:
				t.Fatalf("broker reported Sent before accepted: %v", err)
			default:
			}
			close(release)
			err = <-done
			if loseAck && !errors.Is(err, claudia.ErrReceiptUncertain) {
				t.Fatalf("ack loss not uncertain: %v", err)
			}
			if !loseAck && err != nil {
				t.Fatal(err)
			}
			receipt, err := a.Receipt(context.Background(), "logical-B")
			if err != nil || receipt.AttemptID != msg.AttemptID || receipt.State != claudia.ReceiptAccepted {
				t.Fatalf("query original receipt: %+v %v", receipt, err)
			}
			select {
			case again := <-prompts:
				t.Fatalf("query replayed: %+v", again)
			default:
			}
		})
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

func TestUsageQueryBrokerAndRemoteHandleNeverLoadsSeat(t *testing.T) {
	f := newFixture(t)
	socket := filepath.Join(filepath.Dir(f.sock), "usage.sock")
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
			var msg omp.Message
			if json.NewDecoder(conn).Decode(&msg) != nil {
				_ = conn.Close()
				continue
			}
			seen <- msg
			var n int64 = 17
			fact := omp.UsageFact{Seq: 1, CallID: "call-1", Seat: msg.Seat, SessionID: msg.SessionID, Kind: "model", State: "started", Provider: "mock", Model: "mock", Requests: []omp.UsageRequestRef{{RequestID: "logical", AttemptID: "attempt"}}, StartedAt: "2026-10-16T01:00:00Z", CounterSource: "adapter_normalized_positive", Coverage: "engine_invocation"}
			final := fact
			final.Seq = 2
			final.State = "completed"
			final.FinishedAt = "2026-10-16T01:00:01Z"
			final.Counters.InputTokens = &n
			facts := []omp.UsageFact{fact, final}
			page := &omp.UsagePage{Seat: msg.Seat, SessionID: msg.SessionID, Facts: facts[msg.After:], Next: 2}
			ev := omp.Event{Type: "usage_page", RPCID: msg.RPCID, UsagePage: page}
			if msg.Seat == "corrupt" {
				ev.Type = "error"
				ev.Text = "unreadable journal"
				ev.UsagePage = nil
			}
			_ = json.NewEncoder(conn).Encode(ev)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	f.boot(t, nil)
	q := claudia.UsageQuery{Seat: "unloaded", SessionID: "old"}
	for _, query := range []func(context.Context, claudia.UsageQuery) (*claudia.UsagePage, error){claudia.QueryUsageDirect, claudia.QueryUsage} {
		page, err := query(context.Background(), q)
		if err != nil || len(page.Facts) != 2 {
			t.Fatalf("query: %+v %v", page, err)
		}
		msg := <-seen
		if msg.Op != "usage" || msg.RPCID == "" || msg.SessionID != "old" || msg.Seat != "unloaded" || msg.Token != "" || msg.Provider != "" || msg.RequestID != "" {
			t.Fatalf("query confused identities or loaded credentials: %+v", msg)
		}
	}
	if f.seatCount() != 0 {
		t.Fatal("usage query launched a provider seat")
	}
	q.Seat = "corrupt"
	if page, err := claudia.QueryUsage(context.Background(), q); page != nil || err == nil {
		t.Fatalf("corrupt journal became zero: %+v %v", page, err)
	}
	<-seen
	a, err := claudia.Start(claudia.Config{Name: "seat", SessionID: "generation", WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if !a.DaemonHeld() {
		t.Fatal("not a broker handle")
	}
	if err := a.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-seen
	if err := a.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-seen
	if a.Usage().InputTokens != 17 || a.UsageError() != nil {
		t.Fatalf("remote projection not deduplicated: %+v %v", a.Usage(), a.UsageError())
	}
}

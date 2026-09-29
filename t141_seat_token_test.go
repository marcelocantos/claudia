// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// t141Sidecar is a fake sidecar: it answers loads with ready, records every
// message in order, and lets the test push events at the seat.
type t141Sidecar struct {
	got  chan omp.Message
	conn chan net.Conn
}

func startT141Sidecar(t *testing.T) *t141Sidecar {
	t.Helper()
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-t141-%d-%d.sock", os.Getpid(), time.Now().UnixNano()%100000))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &t141Sidecar{got: make(chan omp.Message, 64), conn: make(chan net.Conn, 1)}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		s.conn <- c
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var msg omp.Message
			_ = json.Unmarshal(line, &msg)
			s.got <- msg
			if msg.Op == omp.OpLoad {
				_, _ = c.Write([]byte("{\"type\":\"ready\"}\n"))
			}
		}
	}()
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)
	return s
}

func t141Write(t *testing.T, c net.Conn, ev string) {
	t.Helper()
	if _, err := c.Write([]byte(ev + "\n")); err != nil {
		t.Fatal(err)
	}
}

// reject makes the provider refuse the seat's token, as a 401 does.
func t141Reject(t *testing.T, c net.Conn) {
	t141Write(t, c, `{"type":"error","text":"401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"OAuth access token has been revoked.\"}}"}`)
}

// nextLoad blocks until the seat sends a load; `go test -timeout` is the clock.
func (s *t141Sidecar) nextLoad() omp.Message {
	for m := range s.got {
		if m.Op == omp.OpLoad {
			return m
		}
	}
	return omp.Message{}
}

// settle proves the seat reloaded nothing since the last load: a marker event
// shows its pump has finished with everything before it, and a sentinel
// prompt sent afterwards must be the next message the sidecar sees — a reload
// from the pump would have been written ahead of it.
func (s *t141Sidecar) settle(t *testing.T, a *Agent, c net.Conn) {
	t.Helper()
	seen := make(chan struct{}, 1)
	tok := a.SubscribeEvents(func(ev Event) {
		if ev.Type == "assistant" && ev.Text == "t141-marker" {
			select {
			case seen <- struct{}{}:
			default:
			}
		}
	})
	defer a.UnsubscribeEvents(tok)
	t141Write(t, c, `{"type":"text","text":"t141-marker"}`)
	<-seen
	if err := a.Send("t141-sentinel"); err != nil {
		t.Fatal(err)
	}
	for m := range s.got {
		if m.Op == omp.OpLoad {
			t.Fatalf("the seat reloaded: %+v", m)
		}
		if m.Op == omp.OpPrompt {
			return
		}
	}
}

func t141Plan(t *testing.T, token string, refreshes *atomic.Int32, refreshed string) {
	t.Helper()
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"r","access_token":"` + token + `","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			refreshes.Add(1)
			if refreshed == "" {
				return nil, fmt.Errorf(`anthropic token refresh failed: 400 {"error": "invalid_grant"}`)
			}
			return []byte(`{"refresh_token":"r2","access_token":"` + refreshed + `","expiry":"` + exp + `"}`), nil
		},
	}
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func startT141Seat(t *testing.T, s *t141Sidecar) (*Agent, net.Conn) {
	t.Helper()
	agent, err := StartDirect(Config{
		Name: "po", Provider: Provider(omp.Anthropic), Model: "claude-sonnet",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.nextLoad()
	return agent, <-s.conn
}

// 🎯T141: the 2026-09-29 sequence. The seat's refresh fails (invalid_grant)
// before the owner repairs the plan; after the repair, the seat's next
// rejection must take the repaired token. The old once-per-lifetime guard
// left it on the revoked token for good.
func TestT141RejectedSeatTakesThePlansRepairedToken(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "revoked", &refreshes, "")
	agent, conn := startT141Seat(t, s)

	t141Reject(t, conn)
	s.settle(t, agent, conn)
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want the one attempt", refreshes.Load())
	}

	// The owner repairs the plan (auth-recover writes the store).
	if err := planStore().Put(context.Background(), omp.Anthropic, omp.Record{
		AccessToken: "repaired", RefreshToken: "r3", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	t141Reject(t, conn)
	m := s.nextLoad()
	if m.Token != "repaired" || m.Model != "claude-sonnet" {
		t.Fatalf("after the repair the seat was not reloaded with it: %+v", m)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d; a repaired token is taken without another refresh", refreshes.Load())
	}
}

// Rejections in a burst refresh once, not once each.
func TestT141RefreshIsRateLimited(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "revoked", &refreshes, "")
	agent, conn := startT141Seat(t, s)
	for range 3 {
		t141Reject(t, conn)
	}
	s.settle(t, agent, conn)
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want 1 within the backoff", n)
	}
}

// A successful plan recovery reaches the seats already running, and keeps
// the model the seat had switched to.
func TestT141RecoverReloadsLiveSeats(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "revoked", &refreshes, "recovered")
	agent, _ := startT141Seat(t, s)
	if err := agent.SetModel("claude-opus"); err != nil {
		t.Fatal(err)
	}
	if m := s.nextLoad(); m.Model != "claude-opus" {
		t.Fatalf("setModel load = %+v", m)
	}
	if err := RecoverOMPPlan(context.Background(), omp.Anthropic); err != nil {
		t.Fatal(err)
	}
	m := s.nextLoad()
	if m.Token != "recovered" {
		t.Fatalf("recovery did not reload the live seat: %+v", m)
	}
	if m.Model != "claude-opus" {
		t.Fatalf("the reload reverted the seat's model to %q", m.Model)
	}
}

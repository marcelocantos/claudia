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
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T159: a token change reaches a sidecar that holds one token per plan as
// a single OpToken naming the plan, not a load per seat; a seat on an older
// sidecar is still reloaded on its own.
func TestT159PlanTokenIsOneMessageNotALoadPerSeat(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"records":{"anthropic":{"refresh_token":"r1","access_token":"fresh","expiry":"` + exp + `"}}}`), nil
	}
	t.Cleanup(func() { ompKeychain = nil; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []omp.Message
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-t159-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var m omp.Message
					_ = json.Unmarshal(line, &m)
					mu.Lock()
					got = append(got, m)
					mu.Unlock()
				}
			}()
		}
	}()
	seat := func(name string, shared bool) *ompControl {
		conn, err := omp.Dial(context.Background(), socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		c := &ompControl{conn: conn, token: "old", provider: omp.Anthropic, seat: name, planTokens: shared}
		ompSeats.Store(c, struct{}{})
		t.Cleanup(func() { ompSeats.Delete(c) })
		return c
	}
	a, b, legacy := seat("po", true), seat("worker", true), seat("old-sidecar-seat", false)

	if n := reloadOMPSeats(context.Background(), omp.Anthropic); n != 3 {
		t.Fatalf("seats moved = %d, want 3", n)
	}
	for _, c := range []*ompControl{a, b, legacy} {
		if c.currentToken() != "fresh" {
			t.Fatalf("%s holds %q", c.seat, c.currentToken())
		}
	}
	want := func() (tokens, loads int) {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range got {
			switch m.Op {
			case omp.OpToken:
				if m.Provider != omp.Anthropic || m.Token != "fresh" || m.Seat != "" {
					t.Fatalf("token message %+v", m)
				}
				tokens++
			case omp.OpLoad:
				if m.Seat != "old-sidecar-seat" {
					t.Fatalf("a shared-token seat was reloaded: %+v", m)
				}
				loads++
			}
		}
		return
	}
	// The sidecar reads asynchronously; wait for both messages to land.
	for {
		if tokens, loads := want(); tokens == 1 && loads == 1 {
			break
		} else if tokens > 1 || loads > 1 {
			t.Fatalf("tokens=%d loads=%d, want one of each", tokens, loads)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Nothing further to send once every seat holds the plan's token.
	if n := reloadOMPSeats(context.Background(), omp.Anthropic); n != 0 {
		t.Fatalf("a second pass moved %d seats", n)
	}
}

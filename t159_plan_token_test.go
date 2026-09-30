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

// 🎯T159: a token change reaches the sidecar as a single OpToken naming the
// plan, not a load per seat. There is no older sidecar to fall back for: a
// broker only ever serves seats through its own (🎯T166).
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
	seat := func(name string) *ompControl {
		conn, err := omp.Dial(context.Background(), socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		c := &ompControl{conn: conn, token: "old", provider: omp.Anthropic, seat: name}
		ompSeats.Store(c, struct{}{})
		t.Cleanup(func() { ompSeats.Delete(c) })
		return c
	}
	a, b, c := seat("po"), seat("worker"), seat("reviewer")

	if n := reloadOMPSeats(context.Background(), omp.Anthropic); n != 3 {
		t.Fatalf("seats moved = %d, want 3", n)
	}
	for _, c := range []*ompControl{a, b, c} {
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
				t.Errorf("a seat was reloaded: %+v", m)
				loads++
			}
		}
		return
	}
	// The sidecar reads asynchronously; wait for the message to land.
	for {
		if tokens, loads := want(); tokens == 1 && loads == 0 {
			break
		} else if tokens > 1 || loads > 0 {
			t.Fatalf("tokens=%d loads=%d, want one token and no loads", tokens, loads)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Nothing further to send once every seat holds the plan's token.
	if n := reloadOMPSeats(context.Background(), omp.Anthropic); n != 0 {
		t.Fatalf("a second pass moved %d seats", n)
	}
}

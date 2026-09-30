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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T158: the 2026-09-30 11:02 sequence. Two seats on one plan are refused
// together after a broker restart. Anthropic's refresh token works once, so
// two refreshes that both present it leave the loser with invalid_grant,
// which flagged a plan whose fresh login was saved and working as rejected
// and asked the owner to sign in again. One refresh must serve both.
func TestT158SeatsRefusedTogetherRefreshThePlanOnce(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"r1","access_token":"old","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }
	var refreshes atomic.Int32
	var spentMu sync.Mutex
	spent := map[string]bool{}
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			refreshes.Add(1)
			var rec struct {
				RefreshToken string `json:"refresh_token"`
			}
			_ = json.Unmarshal([]byte(args[len(args)-1]), &rec)
			spentMu.Lock()
			defer spentMu.Unlock()
			if spent[rec.RefreshToken] {
				return nil, fmt.Errorf(`anthropic token refresh failed: 400 {"error": "invalid_grant"}`)
			}
			spent[rec.RefreshToken] = true
			// Slow enough that an unserialised second refresh overlaps it.
			time.Sleep(50 * time.Millisecond)
			return []byte(`{"refresh_token":"r2","access_token":"fresh","expiry":"` + exp + `"}`), nil
		},
	}
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A sidecar that drains whatever the seats send.
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-t158-%d.sock", os.Getpid()))
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
					if _, err := r.ReadBytes('\n'); err != nil {
						return
					}
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
	a, b, idle := seat("cl-t73-frame-cap"), seat("cl-t130-grok-final"), seat("claudia-po")

	var wg sync.WaitGroup
	for _, c := range []*ompControl{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.recoverRejectedToken()
		}()
	}
	wg.Wait()

	if n := refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want 1 for two seats refused together", n)
	}
	for _, c := range []*ompControl{a, b, idle} {
		if got := c.currentToken(); got != "fresh" {
			t.Fatalf("%s holds %q, want the refreshed token", c.seat, got)
		}
	}
	health, err := omp.Health(context.Background(), planStore())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range health {
		if h.Provider == omp.Anthropic && h.State != omp.HealthOK {
			t.Fatalf("anthropic health %s (%s), want ok", h.State, strings.TrimSpace(h.Detail))
		}
	}
}

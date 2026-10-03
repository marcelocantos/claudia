// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

func TestT169BrokerRetryReceipt(t *testing.T) {
	for _, scenario := range []string{"renewed", "concurrent", "unchanged", "failed", "expired", "unknown-expiry"} {
		t.Run(scenario, func(t *testing.T) {
			var refreshes atomic.Int32
			s := startT141Sidecar(t)
			replacement := "new"
			if scenario == "unchanged" {
				replacement = "old"
			}
			if scenario == "failed" {
				replacement = ""
			}
			t141Plan(t, "old", &refreshes, replacement)
			_, conn := startT141Seat(t, s)
			if scenario == "concurrent" || scenario == "expired" || scenario == "unknown-expiry" {
				expiry := time.Now().Add(time.Hour)
				if scenario == "expired" {
					expiry = time.Now().Add(-time.Hour)
				}
				if scenario == "unknown-expiry" {
					expiry = time.Time{}
				}
				if err := planStore().Put(context.Background(), omp.Anthropic, omp.Record{AccessToken: "new", Expiry: expiry}); err != nil {
					t.Fatal(err)
				}
				if scenario == "expired" {
					ompLogin.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, context.Canceled }
				}
			}
			raw, _ := json.Marshal(omp.Event{Type: "auth_retry", TurnID: "turn", RequestID: "request", FailedToken: tokenFingerprint("old"), Error: "401 OAuth access token has expired"})
			t141Write(t, conn, string(raw))
			for {
				select {
				case m := <-s.got:
					if m.Op != omp.OpAuthRetry {
						continue
					}
					if m.Seat != "po" || m.RequestID != "request" || m.TurnID != "turn" {
						t.Fatalf("lost retry identity")
					}
					want := scenario == "renewed" || scenario == "concurrent"
					if want != (m.Token == "new" && m.ExpiresAt > 0) {
						t.Fatalf("replacement validity wrong for %s", scenario)
					}
					if !want && m.Token != "" {
						t.Fatal("failed recovery authorized replay")
					}
					if scenario == "concurrent" && refreshes.Load() != 0 {
						t.Fatal("concurrent replacement rotated again")
					}
					return
				case <-t.Context().Done():
					t.Fatal("no retry response")
				}
			}
		})
	}
}

func TestT169OwnerExpiryFixture(t *testing.T) {
	if !oauthRejected("401 OAuth access token has expired") {
		t.Fatal("owner expiry refusal missed")
	}
	for _, s := range []string{"429 rate limit", "503 unavailable", "prompt too long"} {
		if oauthRejected(s) {
			t.Fatalf("non-auth refusal matched: %s", s)
		}
	}
}

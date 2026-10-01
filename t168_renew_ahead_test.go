// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// t168Plan stores an anthropic login whose access token expires at exp.
func t168Plan(t *testing.T, exp time.Time, refreshes *atomic.Int32, fail error) {
	t.Helper()
	blob := `{"records":{"anthropic":{"refresh_token":"r","access_token":"old","expiry":"` + exp.UTC().Format(time.RFC3339) + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }
	later := time.Now().Add(8 * time.Hour).UTC().Format(time.RFC3339)
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			refreshes.Add(1)
			if fail != nil {
				return nil, fail
			}
			if len(args) > 1 && args[1] != "refresh" {
				return nil, fmt.Errorf("renewal ran %q, want refresh", args[1])
			}
			return []byte(`{"refresh_token":"r2","access_token":"renewed","expiry":"` + later + `"}`), nil
		},
	}
	omp.ResetKeychainShot()
	ompRenewFailedMu.Lock()
	clear(ompRenewFailed)
	ompRenewFailedMu.Unlock()
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 🎯T168: a token about to expire is renewed before any seat is refused, and
// every seat on the plan takes the new one. On 2026-10-01 nothing renewed a
// token ahead of expiry, and the owner's first message after an idle day was
// refused with "OAuth access token has expired".
func TestT168PlanExpiringSoonIsRenewedAndSeatsReload(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t168Plan(t, time.Now().Add(3*time.Minute), &refreshes, nil)
	startT141Seat(t, s)

	renewed, err := RenewExpiringOMPPlans(context.Background(), OMPRenewMargin)
	if err != nil {
		t.Fatal(err)
	}
	if len(renewed) != 1 || renewed[0] != omp.Anthropic || refreshes.Load() != 1 {
		t.Fatalf("renewed = %v after %d refreshes, want anthropic once", renewed, refreshes.Load())
	}
	if tok, err := planStore().AccessToken(context.Background(), omp.Anthropic); err != nil || tok != "renewed" {
		t.Fatalf("plan token = %q, %v", tok, err)
	}
	// The seat is handed the new token without being refused first.
	for m := range s.got {
		if (m.Op == omp.OpToken || m.Op == omp.OpLoad) && m.Token == "renewed" {
			break
		}
	}
	// Renewed, the plan is no longer due: a second pass does nothing.
	if renewed, err := RenewExpiringOMPPlans(context.Background(), OMPRenewMargin); err != nil || len(renewed) != 0 || refreshes.Load() != 1 {
		t.Fatalf("second pass renewed %v (%v), refreshes = %d", renewed, err, refreshes.Load())
	}
}

func TestT168PlanOutsideMarginIsLeftAlone(t *testing.T) {
	var refreshes atomic.Int32
	t168Plan(t, time.Now().Add(2*time.Hour), &refreshes, nil)
	renewed, err := RenewExpiringOMPPlans(context.Background(), OMPRenewMargin)
	if err != nil || len(renewed) != 0 || refreshes.Load() != 0 {
		t.Fatalf("renewed %v (%v) after %d refreshes, want nothing", renewed, err, refreshes.Load())
	}
}

// A renewal that needs a sign-in marks the plan rejected and is not retried
// every tick; it never opens one itself.
func TestT168RefusedRenewalMarksRejectedAndBacksOff(t *testing.T) {
	var refreshes atomic.Int32
	t168Plan(t, time.Now().Add(time.Minute), &refreshes, fmt.Errorf(`anthropic token refresh failed: 400 {"error": "invalid_grant"}`))
	if _, err := RenewExpiringOMPPlans(context.Background(), OMPRenewMargin); err == nil {
		t.Fatal("a refused renewal reported success")
	}
	health, err := OMPPlanHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rejected := false
	for _, h := range health {
		if h.Provider == omp.Anthropic && h.State == omp.HealthRejected {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("plan not marked rejected: %+v", health)
	}
	if _, err := RenewExpiringOMPPlans(context.Background(), OMPRenewMargin); err != nil || refreshes.Load() != 1 {
		t.Fatalf("second pass: %v, refreshes = %d, want the one attempt", err, refreshes.Load())
	}
}

// A transient failure is retried after the backoff, not every tick.
func TestT168TransientFailureBacksOff(t *testing.T) {
	var refreshes atomic.Int32
	t168Plan(t, time.Now().Add(time.Minute), &refreshes, fmt.Errorf("dial tcp: network is unreachable"))
	if _, err := RenewExpiringOMPPlans(context.Background(), OMPRenewMargin); err == nil {
		t.Fatal("a failed renewal reported success")
	}
	if _, err := RenewExpiringOMPPlans(context.Background(), OMPRenewMargin); err != nil || refreshes.Load() != 1 {
		t.Fatalf("second pass: %v, refreshes = %d, want the one attempt", err, refreshes.Load())
	}
	ompRenewFailedMu.Lock()
	ompRenewFailed[omp.Anthropic] = time.Now().Add(-ompRenewRetry)
	ompRenewFailedMu.Unlock()
	_, _ = RenewExpiringOMPPlans(context.Background(), OMPRenewMargin)
	if refreshes.Load() != 2 {
		t.Fatalf("refreshes = %d after the backoff, want a retry", refreshes.Load())
	}
}

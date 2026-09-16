// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseGrokBillingFixture pins the mapping against a real captured
// x.ai/billing response — most importantly that creditUsagePercent is USED
// (100 → 0% remaining), the polarity that a guess would get backwards.
func TestParseGrokBillingFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/grok/billing_supergrok.json")
	if err != nil {
		t.Fatal(err)
	}
	pu := parseGrokBilling(raw, time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC))
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("status=%q reason=%q, want available", pu.Status, pu.Reason)
	}
	if pu.PlanType != "SuperGrok Heavy" {
		t.Errorf("PlanType=%q, want SuperGrok Heavy", pu.PlanType)
	}
	if len(pu.Windows) != 1 || pu.Windows[0].Name != PlanWindowWeekly {
		t.Fatalf("windows=%+v, want exactly one weekly window", pu.Windows)
	}
	w := pu.Windows[0]
	if w.UsedPercent == nil || *w.UsedPercent != 100 {
		t.Errorf("UsedPercent=%v, want 100", w.UsedPercent)
	}
	if w.RemainingPercent == nil || *w.RemainingPercent != 0 {
		t.Errorf("RemainingPercent=%v, want 0 (pool exhausted) — polarity check", w.RemainingPercent)
	}
	wantReset := time.Date(2026, 8, 15, 1, 53, 9, 930537000, time.UTC)
	if w.ResetsAt == nil || !w.ResetsAt.Equal(wantReset) {
		t.Errorf("ResetsAt=%v, want %v", w.ResetsAt, wantReset)
	}
}

// TestParseGrokBillingFailLoud: a changed/degraded surface must yield explicit
// unavailable, never a fabricated percentage.
func TestParseGrokBillingFailLoud(t *testing.T) {
	now := time.Now()
	cases := map[string]string{
		"missing percent":  `{"config":{"currentPeriod":{"end":"2026-08-15T01:53:09Z"}},"subscription_tier":"x"}`,
		"out of range":     `{"config":{"creditUsagePercent":150}}`,
		"negative percent": `{"config":{"creditUsagePercent":-5}}`,
		"garbage":          `not json`,
	}
	for name, raw := range cases {
		pu := parseGrokBilling([]byte(raw), now)
		if pu.Status != PlanUsageUnavailable {
			t.Errorf("%s: status=%q, want unavailable", name, pu.Status)
		}
		if len(pu.Windows) != 0 {
			t.Errorf("%s: windows must be empty when unavailable, got %+v", name, pu.Windows)
		}
		if pu.Reason == "" {
			t.Errorf("%s: unavailable must carry a reason", name)
		}
	}
}

// TestGrokPlanUsageHTTPTransport exercises the transport hermetically:
// token load from a temp auth.json, the authenticated GET, and parsing — with a
// stub server, no live grok. Also asserts the auth headers the endpoint requires.
func TestGrokPlanUsageHTTPTransport(t *testing.T) {
	const token = "eyJ" + // a token long enough (>200) to satisfy loadGrokToken
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB" +
		"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	var gotAuth, gotXAI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAI = r.Header.Get("X-XAI-Token-Auth")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":40,` +
			`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-08-15T01:53:09.930537Z"}}}`))
	}))
	defer srv.Close()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath,
		[]byte(`{"https://auth.x.ai::client":{"key":"`+token+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:       ProviderGrok,
		GrokAuthPath:   authPath,
		GrokBillingURL: srv.URL,
		Now:            time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("status=%q reason=%q, want available", pu.Status, pu.Reason)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization=%q, want bearer token", gotAuth)
	}
	if gotXAI != "xai-grok-cli" {
		t.Errorf("X-XAI-Token-Auth=%q, want xai-grok-cli", gotXAI)
	}
	if len(pu.Windows) != 1 || pu.Windows[0].RemainingPercent == nil || *pu.Windows[0].RemainingPercent != 60 {
		t.Errorf("windows=%+v, want one weekly window with 60%% remaining", pu.Windows)
	}
}

// TestGrokPlanUsageLive exercises the real undocumented grok billing endpoint
// with the local grok login token. Gated on CLAUDIA_GROK_LIVE. Unavailable
// (e.g. not logged in / expired token) is acceptable, but any reported number
// must be in range.
func TestGrokPlanUsageLive(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pu, err := QueryPlanUsage(ctx, &PlanUsageArgs{Provider: ProviderGrok})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("grok plan usage: status=%s plan=%q reason=%q windows=%+v", pu.Status, pu.PlanType, pu.Reason, pu.Windows)
	switch pu.Status {
	case PlanUsageAvailable:
		if len(pu.Windows) == 0 {
			t.Error("available but no windows")
		}
		for _, w := range pu.Windows {
			if w.RemainingPercent != nil && (*w.RemainingPercent < 0 || *w.RemainingPercent > 100) {
				t.Errorf("remaining %v out of range", *w.RemainingPercent)
			}
		}
	case PlanUsageUnavailable:
		if pu.Reason == "" {
			t.Error("unavailable must carry a reason")
		}
	}
}

// 🎯T74: a billing 401 rotates the login token through the injected refresher
// and retries with the new bearer; a persistent 401 inside the window does
// not spend a second rotation; a non-401 failure never rotates.
func TestGrokPlanUsage401RotatesTokenAndRetries(t *testing.T) {
	grokRefreshGate.reset()
	t.Cleanup(grokRefreshGate.reset)
	pad := strings.Repeat("A", 210)
	oldTok, newTok := "old-"+pad, "new-"+pad
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		bearers = append(bearers, b)
		if b != newTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":71,` +
			`"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-09-19T01:53:09.930537Z"}}}`))
	}))
	defer srv.Close()
	authPath := filepath.Join(t.TempDir(), "auth.json")
	write := func(tok string) {
		if err := os.WriteFile(authPath, []byte(`{"https://auth.x.ai::client":{"key":"`+tok+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(oldTok)
	rotations := 0
	now := time.Date(2026, 9, 16, 12, 5, 0, 0, time.UTC)
	args := func() *PlanUsageArgs {
		return &PlanUsageArgs{
			Provider: ProviderGrok, GrokAuthPath: authPath, GrokBillingURL: srv.URL, Now: now,
			GrokTokenRefresh: func(context.Context) error {
				rotations++
				write(newTok) // what the CLI does on start with an expired token
				return nil
			},
		}
	}

	pu, err := QueryPlanUsage(context.Background(), args())
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("status=%q reason=%q, want available after rotation", pu.Status, pu.Reason)
	}
	if rotations != 1 || len(bearers) != 2 || bearers[0] != oldTok || bearers[1] != newTok {
		t.Fatalf("rotations=%d bearers=%v, want one rotation and the retry with the new bearer", rotations, bearers)
	}
	if *pu.Windows[0].RemainingPercent != 29 {
		t.Fatalf("remaining=%v, want 29", *pu.Windows[0].RemainingPercent)
	}

	// The rotated token stops working again inside the window: no second
	// rotation, and the 401 is reported as such.
	write(oldTok)
	a := args()
	a.Now = now.Add(time.Minute)
	pu, _ = QueryPlanUsage(context.Background(), a)
	if pu.Status != PlanUsageUnavailable || !strings.Contains(pu.Reason, "401") || rotations != 1 {
		t.Fatalf("inside the window: status=%q reason=%q rotations=%d", pu.Status, pu.Reason, rotations)
	}
	// After the window a rotation may run again.
	a = args()
	a.Now = now.Add(grokRefreshWindow + time.Second)
	pu, _ = QueryPlanUsage(context.Background(), a)
	if pu.Status != PlanUsageAvailable || rotations != 2 {
		t.Fatalf("after the window: status=%q rotations=%d", pu.Status, rotations)
	}

	// A rotation that fails leaves a loud 401 naming the attempt.
	grokRefreshGate.reset()
	write(oldTok)
	a = args()
	a.GrokTokenRefresh = func(context.Context) error { return errors.New("simulated: cannot reach auth.x.ai") }
	pu, _ = QueryPlanUsage(context.Background(), a)
	if pu.Status != PlanUsageUnavailable || !strings.Contains(pu.Reason, "rotation") || !strings.Contains(pu.Reason, "401") {
		t.Fatalf("failed rotation: status=%q reason=%q", pu.Status, pu.Reason)
	}
}

func TestGrokPlanUsageNon401DoesNotRotate(t *testing.T) {
	grokRefreshGate.reset()
	t.Cleanup(grokRefreshGate.reset)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"https://auth.x.ai::client":{"key":"`+strings.Repeat("k", 210)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rotations := 0
	pu, _ := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider: ProviderGrok, GrokAuthPath: authPath, GrokBillingURL: srv.URL,
		Now:              time.Date(2026, 9, 16, 12, 5, 0, 0, time.UTC),
		GrokTokenRefresh: func(context.Context) error { rotations++; return nil },
	})
	if pu.Status != PlanUsageUnavailable || !strings.Contains(pu.Reason, "502") || rotations != 0 {
		t.Fatalf("502: status=%q reason=%q rotations=%d", pu.Status, pu.Reason, rotations)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
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

// TestGrokPlanUsageOptInGate: without the opt-in, Grok stays unavailable with an
// opt-in reason and no invented numbers.
func TestGrokPlanUsageOptInGate(t *testing.T) {
	t.Setenv(grokUsageEnv, "")
	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{Provider: ProviderGrok})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("status=%q, want unavailable without opt-in", pu.Status)
	}
	if !strings.Contains(pu.Reason, "opt-in") {
		t.Errorf("reason=%q, want it to mention opt-in", pu.Reason)
	}
	if len(pu.Windows) != 0 {
		t.Error("windows must be empty when gated")
	}
}

// TestGrokPlanUsageHTTPTransport exercises the full opt-in transport hermetically:
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
		Provider:          ProviderGrok,
		GrokUnstableUsage: true,
		GrokAuthPath:      authPath,
		GrokBillingURL:    srv.URL,
		Now:               time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC),
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
	pu, err := QueryPlanUsage(ctx, &PlanUsageArgs{Provider: ProviderGrok, GrokUnstableUsage: true})
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

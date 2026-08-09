// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeOAuthUsageAvailable(t *testing.T) {
	body := []byte(`{
		"five_hour": {
			"utilization": 56.0,
			"resets_at": "2026-08-09T11:00:00.203429+00:00"
		},
		"seven_day": {
			"utilization": 73.0,
			"resets_at": "2026-08-10T02:00:00.203449+00:00"
		}
	}`)
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	pu, err := parseClaudeOAuthUsage(body, now)
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if pu.Provider != ProviderClaude {
		t.Fatalf("Provider=%q", pu.Provider)
	}
	if len(pu.Windows) != 2 {
		t.Fatalf("Windows=%+v", pu.Windows)
	}
	session := pu.Windows[0]
	if session.Name != PlanWindowSession {
		t.Errorf("session name=%q", session.Name)
	}
	if session.UsedPercent == nil || *session.UsedPercent != 56 {
		t.Errorf("session used=%v", session.UsedPercent)
	}
	if session.RemainingPercent == nil || *session.RemainingPercent != 44 {
		t.Errorf("session remaining=%v", session.RemainingPercent)
	}
	if session.ResetsAt == nil || !session.ResetsAt.Equal(time.Date(2026, 8, 9, 11, 0, 0, 203429000, time.UTC)) {
		t.Errorf("session resets=%v", session.ResetsAt)
	}
	weekly := pu.Windows[1]
	if weekly.Name != PlanWindowWeekly {
		t.Errorf("weekly name=%q", weekly.Name)
	}
	if weekly.UsedPercent == nil || *weekly.UsedPercent != 73 {
		t.Errorf("weekly used=%v", weekly.UsedPercent)
	}
	if weekly.RemainingPercent == nil || *weekly.RemainingPercent != 27 {
		t.Errorf("weekly remaining=%v", weekly.RemainingPercent)
	}
}

func TestParseClaudeOAuthUsageEmptyWindowsUnavailable(t *testing.T) {
	body := []byte(`{"five_hour":null,"seven_day":null}`)
	pu, err := parseClaudeOAuthUsage(body, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("Status=%q", pu.Status)
	}
	if len(pu.Windows) != 0 {
		t.Fatalf("invented windows: %+v", pu.Windows)
	}
	if pu.Reason == "" {
		t.Fatal("expected reason")
	}
}

func TestParseClaudeOAuthUsageInvalidJSONUnavailable(t *testing.T) {
	pu, err := parseClaudeOAuthUsage([]byte(`not-json`), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("Status=%q", pu.Status)
	}
}

func TestParseCodexWhamUsagePrimaryWeeklyOnly(t *testing.T) {
	// Observed 2026-08 when 5h window was lifted: primary is 7d, secondary null.
	body := []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {
				"used_percent": 12.5,
				"limit_window_seconds": 604800,
				"reset_after_seconds": 600000,
				"reset_at": 1786874410
			},
			"secondary_window": null
		}
	}`)
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	pu, err := parseCodexWhamUsage(body, now)
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if pu.PlanType != "pro" {
		t.Errorf("PlanType=%q", pu.PlanType)
	}
	if len(pu.Windows) != 1 {
		t.Fatalf("Windows=%+v", pu.Windows)
	}
	w := pu.Windows[0]
	if w.Name != PlanWindowWeekly {
		t.Errorf("name=%q want weekly", w.Name)
	}
	if w.UsedPercent == nil || *w.UsedPercent != 12.5 {
		t.Errorf("used=%v", w.UsedPercent)
	}
	if w.RemainingPercent == nil || *w.RemainingPercent != 87.5 {
		t.Errorf("remaining=%v", w.RemainingPercent)
	}
	if w.LimitWindow != 7*24*time.Hour {
		t.Errorf("LimitWindow=%v", w.LimitWindow)
	}
	if w.ResetsAt == nil || w.ResetsAt.Unix() != 1786874410 {
		t.Errorf("ResetsAt=%v", w.ResetsAt)
	}
}

func TestParseCodexWhamUsageSessionAndWeekly(t *testing.T) {
	body := []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"primary_window": {
				"used_percent": 40,
				"limit_window_seconds": 18000,
				"reset_at": 1700000000
			},
			"secondary_window": {
				"used_percent": 10,
				"limit_window_seconds": 604800,
				"reset_at": 1700500000
			}
		}
	}`)
	pu, err := parseCodexWhamUsage(body, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q", pu.Status)
	}
	if len(pu.Windows) != 2 {
		t.Fatalf("Windows=%+v", pu.Windows)
	}
	if pu.Windows[0].Name != PlanWindowSession {
		t.Errorf("primary name=%q", pu.Windows[0].Name)
	}
	if pu.Windows[1].Name != PlanWindowWeekly {
		t.Errorf("secondary name=%q", pu.Windows[1].Name)
	}
}

func TestParseCodexWhamUsageNoWindowsUnavailable(t *testing.T) {
	pu, err := parseCodexWhamUsage([]byte(`{"plan_type":"pro","rate_limit":null}`), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("Status=%q", pu.Status)
	}
	if len(pu.Windows) != 0 {
		t.Fatalf("invented windows: %+v", pu.Windows)
	}
}

func TestQueryPlanUsageGrokAndBedrockUnavailable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for _, p := range []Provider{ProviderGrok, ProviderBedrock} {
		pu, err := QueryPlanUsage(ctx, &PlanUsageArgs{Provider: p, Now: now})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if pu.Status != PlanUsageUnavailable {
			t.Errorf("%s Status=%q", p, pu.Status)
		}
		if pu.Reason == "" {
			t.Errorf("%s missing reason", p)
		}
		if len(pu.Windows) != 0 {
			t.Errorf("%s invented windows: %+v", p, pu.Windows)
		}
		if !pu.FetchedAt.Equal(now) {
			t.Errorf("%s FetchedAt=%v", p, pu.FetchedAt)
		}
	}
}

func TestQueryPlanUsageClaudeHTTPHermetic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-beta") != claudeOAuthBetaHeader {
			http.Error(w, "missing beta", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{
			"five_hour":{"utilization":10,"resets_at":"2026-08-09T15:00:00Z"},
			"seven_day":{"utilization":20,"resets_at":"2026-08-15T00:00:00Z"}
		}`))
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:          ProviderClaude,
		HTTPClient:        srv.Client(),
		ClaudeAccessToken: "test-token",
		ClaudeUsageURL:    srv.URL,
		Now:               time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if len(pu.Windows) != 2 {
		t.Fatalf("Windows=%+v", pu.Windows)
	}
	if *pu.Windows[0].RemainingPercent != 90 {
		t.Errorf("session remaining=%v", *pu.Windows[0].RemainingPercent)
	}
	if *pu.Windows[1].RemainingPercent != 80 {
		t.Errorf("weekly remaining=%v", *pu.Windows[1].RemainingPercent)
	}
}

func TestQueryPlanUsageClaudeHTTPUnauthorizedUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:          ProviderClaude,
		HTTPClient:        srv.Client(),
		ClaudeAccessToken: "bad",
		ClaudeUsageURL:    srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("Status=%q", pu.Status)
	}
	if !strings.Contains(pu.Reason, "401") {
		t.Errorf("Reason=%q", pu.Reason)
	}
	if len(pu.Windows) != 0 {
		t.Fatalf("invented windows")
	}
}

func TestQueryPlanUsageCodexHTTPHermetic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer codex-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("ChatGPT-Account-Id") != "acct-1" {
			http.Error(w, "missing account", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{
			"plan_type":"pro",
			"rate_limit":{
				"primary_window":{
					"used_percent":25,
					"limit_window_seconds":18000,
					"reset_at":1700003600
				},
				"secondary_window":{
					"used_percent":50,
					"limit_window_seconds":604800,
					"reset_at":1700600000
				}
			}
		}`))
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:         ProviderCodex,
		HTTPClient:       srv.Client(),
		CodexAccessToken: "codex-tok",
		CodexAccountID:   "acct-1",
		CodexUsageURL:    srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if len(pu.Windows) != 2 {
		t.Fatalf("Windows=%+v", pu.Windows)
	}
	if pu.Windows[0].Name != PlanWindowSession || *pu.Windows[0].RemainingPercent != 75 {
		t.Errorf("session=%+v", pu.Windows[0])
	}
	if pu.Windows[1].Name != PlanWindowWeekly || *pu.Windows[1].RemainingPercent != 50 {
		t.Errorf("weekly=%+v", pu.Windows[1])
	}
}

func TestQueryPlanUsageCodexAuthFile(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	auth := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]string{
			"access_token": "file-tok",
			"account_id":   "file-acct",
		},
	}
	raw, _ := json.Marshal(auth)
	if err := os.WriteFile(authPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var gotAuth, gotAcct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAcct = r.Header.Get("ChatGPT-Account-Id")
		_, _ = w.Write([]byte(`{
			"plan_type":"pro",
			"rate_limit":{
				"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1}
			}
		}`))
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:      ProviderCodex,
		HTTPClient:    srv.Client(),
		CodexAuthPath: authPath,
		CodexUsageURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if gotAuth != "Bearer file-tok" {
		t.Errorf("Authorization=%q", gotAuth)
	}
	if gotAcct != "file-acct" {
		t.Errorf("Account=%q", gotAcct)
	}
}

func TestQueryAllPlanUsageHermetic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/claude", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":1,"resets_at":"2026-08-09T12:00:00Z"},"seven_day":{"utilization":2,"resets_at":"2026-08-16T00:00:00Z"}}`))
	})
	mux.HandleFunc("/codex", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":604800,"reset_at":2}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	all, err := QueryAllPlanUsage(context.Background(), &AllPlanUsageArgs{
		HTTPClient:        srv.Client(),
		ClaudeAccessToken: "t",
		CodexAccessToken:  "t",
		ClaudeUsageURL:    srv.URL + "/claude",
		CodexUsageURL:     srv.URL + "/codex",
		Now:               time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("len=%d", len(all))
	}
	by := map[Provider]PlanUsage{}
	for _, pu := range all {
		by[pu.Provider] = pu
	}
	if by[ProviderClaude].Status != PlanUsageAvailable {
		t.Errorf("claude: %+v", by[ProviderClaude])
	}
	if by[ProviderCodex].Status != PlanUsageAvailable {
		t.Errorf("codex: %+v", by[ProviderCodex])
	}
	if by[ProviderGrok].Status != PlanUsageUnavailable {
		t.Errorf("grok: %+v", by[ProviderGrok])
	}
	if by[ProviderBedrock].Status != PlanUsageUnavailable {
		t.Errorf("bedrock: %+v", by[ProviderBedrock])
	}
}

func TestQueryPlanUsageRequiresProvider(t *testing.T) {
	_, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{})
	if err == nil {
		t.Fatal("expected error")
	}
	_, err = QueryPlanUsage(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil args")
	}
}

func TestClassifyCodexWindow(t *testing.T) {
	cases := []struct {
		sec  int
		want PlanWindowName
	}{
		{18000, PlanWindowSession},
		{5 * 3600, PlanWindowSession},
		{604800, PlanWindowWeekly},
		{7 * 24 * 3600, PlanWindowWeekly},
		{86400, PlanWindowName("86400s")},
	}
	for _, tc := range cases {
		if got := classifyCodexWindow(tc.sec); got != tc.want {
			t.Errorf("classifyCodexWindow(%d)=%q want %q", tc.sec, got, tc.want)
		}
	}
}

func TestRemainingFromUsedClamped(t *testing.T) {
	if remainingFromUsed(0) != 100 {
		t.Fatal()
	}
	if remainingFromUsed(100) != 0 {
		t.Fatal()
	}
	if remainingFromUsed(150) != 0 {
		t.Fatal()
	}
	if remainingFromUsed(-5) != 100 {
		t.Fatal()
	}
}

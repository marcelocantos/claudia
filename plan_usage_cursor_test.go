// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseCursorPeriodUsageAvailable(t *testing.T) {
	body := []byte(`{
		"billingCycleStart": "2026-08-01T00:00:00Z",
		"billingCycleEnd": "2026-09-01T00:00:00Z",
		"membershipType": "ultra",
		"planUsage": {
			"totalPercentUsed": 15.5,
			"autoPercentUsed": 10,
			"apiPercentUsed": 20
		}
	}`)
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	pu := parseCursorPeriodUsage(body, now)
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if pu.Provider != ProviderCursor {
		t.Fatalf("Provider=%q", pu.Provider)
	}
	if pu.PlanType != "ultra" {
		t.Errorf("PlanType=%q", pu.PlanType)
	}
	if len(pu.Windows) != 2 {
		t.Fatalf("Windows=%+v", pu.Windows)
	}
	w := pu.Windows[0]
	api := pu.Windows[1]
	if api.Model != "API" {
		t.Errorf("api model=%q", api.Model)
	}
	if api.UsedPercent == nil || *api.UsedPercent != 20 {
		t.Errorf("api used=%v", api.UsedPercent)
	}
	if api.RemainingPercent == nil || *api.RemainingPercent != 80 {
		t.Errorf("api remaining=%v", api.RemainingPercent)
	}
	if api.ResetsAt == nil || w.ResetsAt == nil || !api.ResetsAt.Equal(*w.ResetsAt) {
		t.Errorf("api resets=%v total resets=%v", api.ResetsAt, w.ResetsAt)
	}
	if w.Name != PlanWindowWeekly {
		t.Errorf("name=%q", w.Name)
	}
	if w.UsedPercent == nil || *w.UsedPercent != 15.5 {
		t.Errorf("used=%v", w.UsedPercent)
	}
	if w.RemainingPercent == nil || *w.RemainingPercent != 84.5 {
		t.Errorf("remaining=%v", w.RemainingPercent)
	}
	if w.ResetsAt == nil || !w.ResetsAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("resets=%v", w.ResetsAt)
	}
	if w.LimitWindow != 31*24*time.Hour {
		t.Errorf("LimitWindow=%s", w.LimitWindow)
	}
}

func TestParseCursorPeriodUsageUnixMs(t *testing.T) {
	body := []byte(`{
		"billingCycleStart": "1769904000000",
		"billingCycleEnd": 1772582400000,
		"planUsage": {"totalPercentUsed": 0}
	}`)
	pu := parseCursorPeriodUsage(body, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if pu.Windows[0].ResetsAt == nil {
		t.Fatal("missing ResetsAt from unix-ms end")
	}
}

func TestParseCursorPeriodUsageMissingPercentUnavailable(t *testing.T) {
	pu := parseCursorPeriodUsage([]byte(`{"planUsage":{}}`), time.Now().UTC())
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("Status=%q", pu.Status)
	}
	if len(pu.Windows) != 0 {
		t.Fatalf("invented windows: %+v", pu.Windows)
	}
	if !strings.Contains(pu.Reason, "totalPercentUsed") {
		t.Errorf("Reason=%q", pu.Reason)
	}
}

func TestParseCursorPeriodUsageOutOfRangeUnavailable(t *testing.T) {
	pu := parseCursorPeriodUsage([]byte(`{"planUsage":{"totalPercentUsed":101}}`), time.Now().UTC())
	if pu.Status != PlanUsageUnavailable || len(pu.Windows) != 0 {
		t.Fatalf("Status=%q windows=%+v", pu.Status, pu.Windows)
	}
}

func TestQueryPlanUsageCursorRawBypassesNetwork(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:       ProviderCursor,
		Now:            now,
		CursorUsageRaw: json.RawMessage(`{"membershipType":"pro","planUsage":{"totalPercentUsed":40}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if *pu.Windows[0].RemainingPercent != 60 {
		t.Errorf("remaining=%v", *pu.Windows[0].RemainingPercent)
	}
	if pu.PlanType != "pro" {
		t.Errorf("PlanType=%q", pu.PlanType)
	}
}

func TestQueryPlanUsageCursorHTTPHermetic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"planUsage":{"totalPercentUsed":25},"membershipType":"ultra"}`))
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:          ProviderCursor,
		HTTPClient:        srv.Client(),
		CursorAccessToken: "test-token",
		CursorUsageURL:    srv.URL,
		Now:               time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
	}
	if *pu.Windows[0].RemainingPercent != 75 {
		t.Errorf("remaining=%v", *pu.Windows[0].RemainingPercent)
	}
}

func TestQueryPlanUsageCursorHTTPUnauthorizedUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:          ProviderCursor,
		HTTPClient:        srv.Client(),
		CursorAccessToken: "bad",
		CursorUsageURL:    srv.URL,
		Now:               time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("Status=%q", pu.Status)
	}
	if !strings.Contains(pu.Reason, "HTTP 401") {
		t.Errorf("Reason=%q", pu.Reason)
	}
}

func TestLoadCursorAccessTokenURIMode(t *testing.T) {
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not on PATH")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	create := exec.Command(sqlite3, path,
		`CREATE TABLE ItemTable (key TEXT, value TEXT); INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', 'test-cursor-token');`)
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v %s", err, out)
	}
	tok, err := loadCursorAccessToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "test-cursor-token" {
		t.Fatalf("token %q", tok)
	}
}

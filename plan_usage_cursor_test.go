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

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
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
	if api.Name != PlanWindowAPI {
		t.Errorf("api name=%q", api.Name)
	}
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

// writeCursorStateDB builds a state.vscdb fixture shaped like the Cursor
// IDE's: ItemTable(key, value) in WAL mode. The returned handle stays open
// until the test ends, so the rows live only in the -wal file, which is
// where a running IDE keeps its recent writes.
func writeCursorStateDB(t *testing.T, path string, rows map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlpipe.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for k, v := range rows {
		if err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// fileDigests reads every file beside path that shares its name, so a
// test can prove the reader wrote none of them.
func fileDigests(t *testing.T, path string) map[string]string {
	t.Helper()
	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(m)] = string(b)
	}
	return out
}

func TestLoadCursorAccessTokenReadsStateDB(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	path := filepath.Join(t.TempDir(), "state.vscdb")
	writeCursorStateDB(t, path, map[string]string{
		"cursorAuth/accessToken":  "fixture-access-token",
		"cursorAuth/refreshToken": "fixture-refresh-token",
	})
	before := fileDigests(t, path)
	if _, ok := before["state.vscdb-wal"]; !ok {
		t.Fatalf("fixture has no WAL; files %v", before)
	}
	// af439b7 stopped plan usage shelling out; the restored read stays
	// in-process. A sqlite3 on PATH that leaves a marker proves it.
	bin := t.TempDir()
	marker := filepath.Join(bin, "marker")
	if err := os.WriteFile(filepath.Join(bin, "sqlite3"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	tok, err := loadCursorAccessToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "fixture-access-token" {
		t.Fatalf("token %q", tok)
	}
	after := fileDigests(t, path)
	if len(after) != len(before) {
		t.Fatalf("reader changed the file set: before %d files, after %d", len(before), len(after))
	}
	for name, b := range before {
		if after[name] != b {
			t.Errorf("reader modified %s", name)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("reader left %d entries in TMPDIR", len(left))
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("reader started sqlite3: %v", statErr)
	}
}

func TestLoadCursorAccessTokenDefaultPathUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	writeCursorStateDB(t, path, map[string]string{"cursorAuth/accessToken": "home-token"})
	tok, err := loadCursorAccessToken("")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "home-token" {
		t.Fatalf("token %q", tok)
	}
}

func TestLoadCursorAccessTokenKeyAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	writeCursorStateDB(t, path, map[string]string{"cursorAuth/cachedEmail": "someone@example.com"})
	tok, err := loadCursorAccessToken(path)
	if err == nil {
		t.Fatalf("no error; token %q", tok)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "cursorAuth/accessToken") {
		t.Fatalf("error does not name the path and key: %v", err)
	}
}

func TestLoadCursorAccessTokenFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "state.vscdb")
	_, err := loadCursorAccessToken(path)
	if err == nil {
		t.Fatal("no error for a missing state.vscdb")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error does not name the path: %v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(path)); !os.IsNotExist(statErr) {
		t.Fatalf("reader created the missing directory: %v", statErr)
	}
}

// The token reaches the billing endpoint from whichever source wins:
// PlanUsageArgs.CursorAccessToken, then CURSOR_API_KEY, then state.vscdb.
func TestQueryPlanUsageCursorTokenPrecedence(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"planUsage":{"totalPercentUsed":30},"membershipType":"pro"}`))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	writeCursorStateDB(t, path, map[string]string{"cursorAuth/accessToken": "db-token"})

	for _, tc := range []struct {
		name, arg, env, want string
	}{
		{"state db", "", "", "Bearer db-token"},
		{"env beats state db", "", "env-token", "Bearer env-token"},
		{"arg beats env", "arg-token", "env-token", "Bearer arg-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen = ""
			t.Setenv(cursorAPIKeyEnv, tc.env)
			pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
				Provider:          ProviderCursor,
				HTTPClient:        srv.Client(),
				CursorAccessToken: tc.arg,
				CursorAuthPath:    path,
				CursorUsageURL:    srv.URL,
				Now:               time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatal(err)
			}
			if pu.Status != PlanUsageAvailable {
				t.Fatalf("Status=%q Reason=%q", pu.Status, pu.Reason)
			}
			if seen != tc.want {
				t.Fatalf("Authorization %q, want %q", seen, tc.want)
			}
		})
	}
}

// A host with no token anywhere gets a reading that says where it looked.
func TestQueryPlanUsageCursorMissingStateDBReasonNamesPath(t *testing.T) {
	t.Setenv(cursorAPIKeyEnv, "")
	path := filepath.Join(t.TempDir(), "state.vscdb")
	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:       ProviderCursor,
		CursorAuthPath: path,
		CursorUsageURL: "http://127.0.0.1:1/unreachable",
		Now:            time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("Status=%q", pu.Status)
	}
	if !strings.Contains(pu.Reason, path) || !strings.Contains(pu.Reason, cursorAPIKeyEnv) {
		t.Fatalf("Reason does not name the path and the env override: %q", pu.Reason)
	}
}

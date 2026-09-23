// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// cursorPeriodUsageURL is the undocumented DashboardService RPC the Cursor
// IDE billing UI reads. Private and unversioned. A break fails loud
// (unavailable + reason), never a fabricated percent.
const cursorPeriodUsageURL = "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage"

const cursorAPIKeyEnv = "CURSOR_API_KEY"

// cursorPeriodUsage is the subset of GetCurrentPeriodUsage this maps.
// Unknown fields are ignored. A missing totalPercentUsed is unavailable,
// not a fabricated 0.
type cursorPeriodUsage struct {
	BillingCycleStart json.RawMessage `json:"billingCycleStart"`
	BillingCycleEnd   json.RawMessage `json:"billingCycleEnd"`
	PlanUsage         *struct {
		TotalPercentUsed *float64 `json:"totalPercentUsed"`
		AutoPercentUsed  *float64 `json:"autoPercentUsed"`
		APIPercentUsed   *float64 `json:"apiPercentUsed"`
		Limit            *int64   `json:"limit"`
	} `json:"planUsage"`
	MembershipType string `json:"membershipType"`
	PlanType       string `json:"planType"`
}

func parseCursorPeriodUsage(raw []byte, now time.Time) PlanUsage {
	var r cursorPeriodUsage
	if err := json.Unmarshal(raw, &r); err != nil {
		return unavailablePlan(ProviderCursor, now, "cursor usage: unparseable response: "+err.Error())
	}
	if r.PlanUsage == nil || r.PlanUsage.TotalPercentUsed == nil {
		return unavailablePlan(ProviderCursor, now,
			"cursor usage: response carries no planUsage.totalPercentUsed (private surface may have changed)")
	}
	used := *r.PlanUsage.TotalPercentUsed
	if used < 0 || used > 100 {
		return unavailablePlan(ProviderCursor, now,
			fmt.Sprintf("cursor usage: totalPercentUsed %.2f is outside 0–100 (surface may have changed)", used))
	}
	w := PlanWindow{
		Name:             PlanWindowWeekly,
		UsedPercent:      floatPtr(used),
		RemainingPercent: floatPtr(remainingFromUsed(used)),
	}
	start := parseCursorTime(r.BillingCycleStart)
	end := parseCursorTime(r.BillingCycleEnd)
	if end != nil {
		w.ResetsAt = end
	}
	if start != nil && end != nil && end.After(*start) {
		w.LimitWindow = end.Sub(*start)
	}
	windows := []PlanWindow{w}
	// totalPercentUsed blends the auto bucket with named-model API usage.
	// A seat on claude-opus or gpt is refused ("Upgrade your plan") when
	// apiPercentUsed hits 100 while the blend is still near half. The API
	// bucket is its own window so the cockpit can show that split.
	if api, ok := cursorAPIWindow(r.PlanUsage.APIPercentUsed, w); ok {
		windows = append(windows, api)
	}
	plan := r.MembershipType
	if plan == "" {
		plan = r.PlanType
	}
	return PlanUsage{
		Provider:  ProviderCursor,
		Status:    PlanUsageAvailable,
		Windows:   windows,
		PlanType:  plan,
		FetchedAt: now,
	}
}

// cursorAPIWindow is the named-model bucket. An absent or out-of-range
// figure is omitted; it must not take down the total reading.
func cursorAPIWindow(used *float64, base PlanWindow) (PlanWindow, bool) {
	if used == nil || *used < 0 || *used > 100 {
		return PlanWindow{}, false
	}
	w := base
	w.Model = "API"
	w.UsedPercent = floatPtr(*used)
	w.RemainingPercent = floatPtr(remainingFromUsed(*used))
	return w, true
}

// parseCursorTime accepts RFC3339, RFC3339Nano, or a unix-ms JSON number /
// numeric string (the dashboard has used both).
func parseCursorTime(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return &t
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return &t
		}
		if ms, err := parseInt64String(s); err == nil && ms > 0 {
			t := time.UnixMilli(ms).UTC()
			return &t
		}
		return nil
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err == nil {
		if ms, err := n.Int64(); err == nil && ms > 0 {
			t := time.UnixMilli(ms).UTC()
			return &t
		}
	}
	return nil
}

func parseInt64String(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscan(s, &n)
	return n, err
}

func queryCursorPlanUsage(ctx context.Context, args *PlanUsageArgs, now time.Time) PlanUsage {
	if len(args.CursorUsageRaw) > 0 {
		return parseCursorPeriodUsage(args.CursorUsageRaw, now)
	}
	raw, err := fetchCursorPeriodUsage(ctx, args)
	if err != nil {
		return unavailablePlan(ProviderCursor, now, "cursor usage: "+err.Error())
	}
	return parseCursorPeriodUsage(raw, now)
}

func fetchCursorPeriodUsage(ctx context.Context, args *PlanUsageArgs) ([]byte, error) {
	tok := strings.TrimSpace(args.CursorAccessToken)
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv(cursorAPIKeyEnv))
	}
	if tok == "" {
		t, err := loadCursorAccessToken(args.CursorAuthPath)
		if err != nil {
			return nil, err
		}
		tok = t
	}
	if tok == "" {
		return nil, fmt.Errorf("no access token (set %s, PlanUsageArgs.CursorAccessToken, or sign in to Cursor)", cursorAPIKeyEnv)
	}
	url := args.CursorUsageURL
	if url == "" {
		url = cursorPeriodUsageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claudia/"+Version)

	client := args.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d (token may be expired — run `agent login`)", resp.StatusCode)
	}
	return body, nil
}

// loadCursorAccessToken reads the Cursor IDE session token from
// state.vscdb (key cursorAuth/accessToken) via sqlite3(1). The CLI's
// CURSOR_API_KEY is preferred; this is a fallback for hosts already
// signed into the desktop app. CursorAuthPath overrides the db path.
func loadCursorAccessToken(path string) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home for cursor auth: %w", err)
		}
		path = filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
		}
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("read cursor auth (set %s or run `agent login`): %w", cursorAPIKeyEnv, err)
	}
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		return "", fmt.Errorf("sqlite3 not on PATH to read %s (set %s instead)", path, cursorAPIKeyEnv)
	}
	// Apple's /usr/bin/sqlite3 -readonly cannot open Cursor's state.vscdb
	// (SQLITE_CANTOPEN / exit 14). URI mode=ro opens the same file.
	uri := "file:" + path + "?mode=ro"
	cmd := exec.Command(sqlite3, uri, `SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken' LIMIT 1;`)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read cursor auth db: %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("no cursorAuth/accessToken in %s (set %s or sign in to Cursor)", path, cursorAPIKeyEnv)
	}
	return tok, nil
}

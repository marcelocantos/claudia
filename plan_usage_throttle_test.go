// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 🎯T85 / 🎯T84: the vendor counts requests per account, not per process,
// and every response is worth keeping.

const t85Body = `{"five_hour":{"utilization":33,"resets_at":"2026-09-20T18:00:00Z"},` +
	`"seven_day":{"utilization":13,"resets_at":"2026-09-26T00:00:00Z"},` +
	`"seven_day_fable_five":{"utilization":100,"resets_at":"2026-09-26T00:00:00Z"}}`

func t85Pct(v float64) *float64 { return &v }

func t85At(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A restart must not buy a fresh allowance: the floor lives on disk, so a
// second process asking a second later is refused without a request.
func TestT85ThrottleSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	base := t85At(t, "2026-09-20T12:00:00Z")

	if d := CheckPlanThrottle(dir, ProviderClaude, base, false); !d.Allowed {
		t.Fatalf("first request refused: %s", d.Reason)
	}
	RecordPlanAttempt(dir, ProviderClaude, base, http.StatusOK)

	// "Restart": nothing in memory, only the file on disk.
	d := CheckPlanThrottle(dir, ProviderClaude, base.Add(time.Second), false)
	if d.Allowed {
		t.Fatal("a restart one second later was allowed to re-ask the vendor")
	}
	if d.RetryAt.Before(base.Add(PlanThrottleMinInterval)) {
		t.Fatalf("RetryAt %s is inside the floor", d.RetryAt)
	}
	// Past the floor it opens again.
	if d := CheckPlanThrottle(dir, ProviderClaude, base.Add(PlanThrottleMinInterval+time.Second), false); !d.Allowed {
		t.Fatalf("still refused past the floor: %s", d.Reason)
	}
	// One provider's floor says nothing about another's.
	if d := CheckPlanThrottle(dir, ProviderGrok, base.Add(time.Second), false); !d.Allowed {
		t.Fatalf("grok refused on claude's account: %s", d.Reason)
	}
}

// A refusal is not retried on a short ladder: the endpoint sends no
// Retry-After, and asking again is what extends the refusal.
func TestT85RefusalEscalatesAndForcedRefreshCannotWaiveIt(t *testing.T) {
	dir := t.TempDir()
	base := t85At(t, "2026-09-20T12:00:00Z")

	RecordPlanAttempt(dir, ProviderClaude, base, http.StatusTooManyRequests)
	d := CheckPlanThrottle(dir, ProviderClaude, base.Add(time.Minute), true)
	if d.Allowed {
		t.Fatal("a forced refresh punched through a rate-limit penalty")
	}
	if want := base.Add(PlanThrottleFirstPenalty); !d.RetryAt.Equal(want) {
		t.Fatalf("first penalty ends %s, want %s", d.RetryAt, want)
	}

	// A second refusal doubles rather than repeating.
	second := base.Add(PlanThrottleFirstPenalty + time.Minute)
	RecordPlanAttempt(dir, ProviderClaude, second, http.StatusTooManyRequests)
	d = CheckPlanThrottle(dir, ProviderClaude, second.Add(time.Minute), false)
	if want := second.Add(2 * PlanThrottleFirstPenalty); !d.RetryAt.Equal(want) {
		t.Fatalf("second penalty ends %s, want %s", d.RetryAt, want)
	}

	// Any answer at all clears it: the provider is no longer refusing.
	third := second.Add(3 * time.Hour)
	RecordPlanAttempt(dir, ProviderClaude, third, http.StatusOK)
	if d := CheckPlanThrottle(dir, ProviderClaude, third.Add(PlanThrottleMinInterval+time.Second), false); !d.Allowed {
		t.Fatalf("penalty outlived a successful response: %s", d.Reason)
	}
}

// The penalty is capped, so a long outage cannot push the next attempt
// beyond any useful horizon.
func TestT85PenaltyIsCapped(t *testing.T) {
	dir := t.TempDir()
	at := t85At(t, "2026-09-20T12:00:00Z")
	for i := 0; i < 12; i++ {
		RecordPlanAttempt(dir, ProviderClaude, at, http.StatusTooManyRequests)
	}
	d := CheckPlanThrottle(dir, ProviderClaude, at, false)
	if got := d.RetryAt.Sub(at); got > PlanThrottleMaxPenalty {
		t.Fatalf("penalty %s exceeds the %s cap", got, PlanThrottleMaxPenalty)
	}
}

// The state file is the owner's own and carries no secret, but it is
// account-shaped: it stays private.
func TestT85ThrottleFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	RecordPlanAttempt(dir, ProviderClaude, time.Now(), http.StatusOK)
	st, err := os.Stat(filepath.Join(dir, planThrottleFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("throttle file mode %o, want 600", perm)
	}
}

// A corrupt file costs at most one early request; it never takes plan
// usage down.
func TestT85CorruptThrottleFileDoesNotBreakUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, planThrottleFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := CheckPlanThrottle(dir, ProviderClaude, time.Now(), false); !d.Allowed {
		t.Fatalf("corrupt state refused a request: %s", d.Reason)
	}
}

// 🎯T84: the response body reaches the caller, including fields this
// package does not map — which is the whole point, since an unmapped
// per-model window is exactly what went unnoticed for weeks.
func TestT84RawResponseRidesHomeOnTheReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, t85Body)
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:          ProviderClaude,
		ClaudeAccessToken: "test-token",
		ClaudeUsageURL:    srv.URL,
		Now:               t85At(t, "2026-09-20T12:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.HTTPStatus != http.StatusOK {
		t.Fatalf("HTTPStatus = %d, want 200", pu.HTTPStatus)
	}
	if pu.RawBody != t85Body {
		t.Fatalf("RawBody did not survive:\n got %q\nwant %q", pu.RawBody, t85Body)
	}
	// The unmapped window is present in the record even though no window
	// was built from it.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(pu.RawBody), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["seven_day_fable_five"]; !ok {
		t.Fatal("the unmapped per-model window was dropped from the record")
	}
	// Parsing still worked: recording must not consume the body.
	if len(pu.Windows) == 0 {
		t.Fatal("recording the body left the parser with nothing to read")
	}
}

// A refusal is recorded as a refusal, not as a reading.
func TestT84RefusalKeepsItsBodyAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:          ProviderClaude,
		ClaudeAccessToken: "test-token",
		ClaudeUsageURL:    srv.URL,
		Now:               t85At(t, "2026-09-20T12:00:00Z"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want 429", pu.HTTPStatus)
	}
	if pu.Status != PlanUsageUnavailable {
		t.Fatalf("a 429 produced status %q, want unavailable", pu.Status)
	}
	if len(pu.Windows) != 0 {
		t.Fatal("a refused reading produced windows; unknown is not zero")
	}
}

// 🎯T85: a withheld provider keeps its previous reading rather than
// blanking, and the reason says why the number is not moving.
func TestT85ThrottledProviderCarriesItsLastReadingForward(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, planCacheSnapshotFile)
	earlier := t85At(t, "2026-09-20T11:00:00Z")
	prev := planCacheSnapshot{FetchedAt: earlier, Backends: []PlanUsage{{
		Provider:  ProviderClaude,
		Status:    PlanUsageAvailable,
		FetchedAt: earlier,
		RawBody:   t85Body,
		Windows:   []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: t85Pct(41)}},
	}}}
	if err := writePlanSnapshot(snapPath, prev); err != nil {
		t.Fatal(err)
	}

	fresh := []PlanUsage{{Provider: ProviderGrok, Status: PlanUsageAvailable}}
	out := carryForwardThrottled(snapPath, fresh,
		map[Provider]string{ProviderClaude: "holding off until 12:15"})

	var claude *PlanUsage
	for i := range out {
		if out[i].Provider == ProviderClaude {
			claude = &out[i]
		}
	}
	if claude == nil {
		t.Fatal("the withheld provider vanished from the snapshot")
	}
	if len(claude.Windows) != 1 || claude.Windows[0].RemainingPercent == nil ||
		*claude.Windows[0].RemainingPercent != 41 {
		t.Fatalf("carried reading lost its windows: %+v", claude.Windows)
	}
	if !claude.FetchedAt.Equal(earlier) {
		t.Fatalf("FetchedAt = %s, want the original %s: age must be visible, not forged",
			claude.FetchedAt, earlier)
	}
	if claude.RawBody != "" {
		t.Fatal("the carried reading kept a body belonging to an earlier request")
	}
	if claude.Reason == "" {
		t.Fatal("nothing says why this provider's number is not moving")
	}
}

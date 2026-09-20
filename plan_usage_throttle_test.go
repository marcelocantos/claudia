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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🎯T85 / 🎯T84: the vendor counts requests per account, not per process,
// and every response is worth keeping.

const t85Body = `{"five_hour":{"utilization":33,"resets_at":"2026-09-20T18:00:00Z"},` +
	`"seven_day":{"utilization":13,"resets_at":"2026-09-26T00:00:00Z"},` +
	`"seven_day_fable_five":{"utilization":100,"resets_at":"2026-09-26T00:00:00Z"}}`

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
	base := t85At(t, "2026-09-20T12:00:00Z")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, t85Body)
	}))
	defer srv.Close()

	all := func(now time.Time) *AllPlanUsageArgs {
		return &AllPlanUsageArgs{
			Providers:         []Provider{ProviderClaude},
			ClaudeAccessToken: "test-token",
			ClaudeUsageURL:    srv.URL,
			Now:               now,
			ThrottleDir:       dir,
			ThrottleSkipped:   map[Provider]string{},
		}
	}

	first := all(base)
	got, err := QueryAllPlanUsage(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != PlanUsageAvailable {
		t.Fatalf("first read: %+v", got)
	}

	// A minute later the floor bites. The gauge must not blank: a hole
	// here is what parked a live worker on 2026-09-20.
	second := all(base.Add(time.Minute))
	got, err = QueryAllPlanUsage(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("the withheld read still reached the vendor: %d requests", hits)
	}
	if len(got) != 1 {
		t.Fatalf("the withheld provider vanished from the reading: %+v", got)
	}
	held := got[0]
	if held.Status != PlanUsageAvailable || len(held.Windows) == 0 {
		t.Fatalf("withheld provider published a hole or a zero: %+v", held)
	}
	if !held.FetchedAt.Equal(base) {
		t.Fatalf("FetchedAt = %s, want the original %s: age must be visible, not forged",
			held.FetchedAt, base)
	}
	if held.RawBody != "" {
		t.Fatal("the carried reading kept a body belonging to an earlier request")
	}
	if held.Reason == "" {
		t.Fatal("nothing says why this provider's number is not moving")
	}
	if second.ThrottleSkipped[ProviderClaude] == "" {
		t.Fatal("the caller was not told which provider was withheld")
	}

	// Past the floor it asks again, and the reading is fresh.
	third := all(base.Add(PlanThrottleMinInterval + time.Second))
	got, err = QueryAllPlanUsage(context.Background(), third)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("past the floor the vendor saw %d requests, want 2", hits)
	}
	if got[0].Reason != "" || !got[0].FetchedAt.Equal(third.Now) {
		t.Fatalf("the fresh reading still looks carried: %+v", got[0])
	}
}

// An explicit refresh is prompt but not unlimited: it shortens the floor,
// it does not remove it, because a reload button is the other way a
// person produces the burst this mechanism exists to prevent.
func TestT85ForcedRefreshStillObeysAShortFloor(t *testing.T) {
	dir := t.TempDir()
	base := t85At(t, "2026-09-20T12:00:00Z")
	RecordPlanAttempt(dir, ProviderClaude, base, http.StatusOK)

	if d := CheckPlanThrottle(dir, ProviderClaude, base.Add(5*time.Second), true); d.Allowed {
		t.Fatal("a refresh five seconds later reached the vendor")
	}
	if d := CheckPlanThrottle(dir, ProviderClaude, base.Add(PlanThrottleForcedInterval+time.Second), true); !d.Allowed {
		t.Fatalf("a refresh past the forced floor was still refused: %s", d.Reason)
	}
	// The unforced floor is still the long one.
	if d := CheckPlanThrottle(dir, ProviderClaude, base.Add(PlanThrottleForcedInterval+time.Second), false); d.Allowed {
		t.Fatal("an ordinary read took the forced floor")
	}
}

// A request that never completed says nothing about the vendor's mood.
// Clearing a penalty on it would let a flaky network hand back the
// allowance the vendor took away.
func TestT85TransportFailureDoesNotClearAPenalty(t *testing.T) {
	dir := t.TempDir()
	base := t85At(t, "2026-09-20T12:00:00Z")
	RecordPlanAttempt(dir, ProviderClaude, base, http.StatusTooManyRequests)
	RecordPlanAttempt(dir, ProviderClaude, base.Add(time.Minute), 0)

	d := CheckPlanThrottle(dir, ProviderClaude, base.Add(2*time.Minute), false)
	if d.Allowed {
		t.Fatal("a transport failure waived the rate-limit penalty")
	}
	if want := base.Add(PlanThrottleFirstPenalty); !d.RetryAt.Equal(want) {
		t.Fatalf("penalty ends %s, want the untouched %s", d.RetryAt, want)
	}
}

// 🎯T85, the headline: a SECOND OS PROCESS, born with no memory, is
// refused the request its predecessor already spent — and the refusal
// costs no request, which the vendor's own hit counter proves.
//
// The child is this test binary re-executed; CLAUDIA_T85_CHILD tells it
// which half to run.
func TestT85SecondProcessIsRefusedWithoutIssuingARequest(t *testing.T) {
	if os.Getenv("CLAUDIA_T85_CHILD") != "" {
		t85ChildFetch(t)
		return
	}
	dir := t.TempDir()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, t85Body)
	}))
	defer srv.Close()

	if _, err := QueryAllPlanUsage(context.Background(), &AllPlanUsageArgs{
		Providers:         []Provider{ProviderClaude},
		ClaudeAccessToken: "test-token",
		ClaudeUsageURL:    srv.URL,
		Now:               time.Now(),
		ThrottleDir:       dir,
	}); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("first process made %d requests, want 1", hits)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestT85SecondProcessIsRefusedWithoutIssuingARequest")
	cmd.Env = append(os.Environ(),
		"CLAUDIA_T85_CHILD=1",
		"CLAUDIA_T85_DIR="+dir,
		"CLAUDIA_T85_URL="+srv.URL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "T85-CHILD-WITHHELD") {
		t.Fatalf("the second process was not refused:\n%s", out)
	}
	if hits != 1 {
		t.Fatalf("the second process issued %d requests; the floor must refuse without asking", hits-1)
	}
}

// t85ChildFetch is the second process: it shares only the state directory
// on disk, and must be refused by it.
func t85ChildFetch(t *testing.T) {
	skipped := map[Provider]string{}
	if _, err := QueryAllPlanUsage(context.Background(), &AllPlanUsageArgs{
		Providers:         []Provider{ProviderClaude},
		ClaudeAccessToken: "test-token",
		ClaudeUsageURL:    os.Getenv("CLAUDIA_T85_URL"),
		Now:               time.Now(),
		ThrottleDir:       os.Getenv("CLAUDIA_T85_DIR"),
		ThrottleSkipped:   skipped,
	}); err != nil {
		t.Fatalf("child fetch failed: %v", err)
	}
	if reason := skipped[ProviderClaude]; reason != "" {
		fmt.Println("T85-CHILD-WITHHELD", reason)
		return
	}
	t.Fatal("the child process was allowed to re-ask the vendor")
}

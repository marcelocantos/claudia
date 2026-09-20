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
	"strings"
	"testing"
	"time"
)

// 🎯T84: the payloads are kept, bounded and private, and the interesting
// one outlives the boring one.

// The store answers "what did the server say?" from disk. Nothing here
// issues a second request, which is the entire reason it exists.
func TestT84PayloadsAreReadBackWithoutARequest(t *testing.T) {
	dir := t.TempDir()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprint(w, t85Body)
	}))
	defer srv.Close()

	at := t85At(t, "2026-09-20T12:00:00Z")
	if _, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:          ProviderClaude,
		ClaudeAccessToken: "test-token",
		ClaudeUsageURL:    srv.URL,
		Now:               at,
		ThrottleDir:       dir,
	}); err != nil {
		t.Fatal(err)
	}

	got := ReadPlanRawPayloads(dir, ProviderClaude)
	if len(got) != 1 {
		t.Fatalf("store holds %d payloads, want 1", len(got))
	}
	if got[0].Body != t85Body {
		t.Fatalf("stored body:\n got %q\nwant %q", got[0].Body, t85Body)
	}
	if !got[0].FetchedAt.Equal(at) {
		t.Fatalf("payload is addressed by fetch time %s, want %s", got[0].FetchedAt, at)
	}
	if got[0].HTTPStatus != http.StatusOK {
		t.Fatalf("stored status %d, want 200", got[0].HTTPStatus)
	}
	if hits != 1 {
		t.Fatalf("reading the store cost %d vendor requests, want 1 (the original fetch)", hits)
	}
}

// The store is the owner's own account data: private, and outside any
// repository.
func TestT84StoreIsPrivateAndOutsideAnyRepo(t *testing.T) {
	dir := t.TempDir()
	RecordPlanRawPayload(dir, ProviderClaude, time.Now(), 200, t85Body)

	file := filepath.Join(dir, planRawStoreDir, string(ProviderClaude)+".json")
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("payload file mode %o, want 600", perm)
	}
	st, err = os.Stat(filepath.Join(dir, planRawStoreDir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Fatalf("store directory mode %o, want 700", perm)
	}

	// The default location is the user cache directory, and nothing on the
	// way up from it is a working tree.
	t.Setenv("CLAUDIA_PLAN_CACHE", "")
	def, err := planCacheDir("")
	if err != nil {
		t.Skipf("no user cache dir on this host: %v", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache dir on this host: %v", err)
	}
	if !strings.HasPrefix(def, cache+string(os.PathSeparator)) {
		t.Fatalf("default store %q is not under the user cache dir %q", def, cache)
	}
	for p := def; ; {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			t.Fatalf("default store %q sits inside the repository at %q", def, p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
}

// The ring is bounded, and it gives up a payload that parsed cleanly
// before one carrying a key no parser reads — a surface change has to
// survive the traffic that follows it.
func TestT84StoreIsBoundedAndKeepsTheUnmappedPayload(t *testing.T) {
	dir := t.TempDir()
	base := t85At(t, "2026-09-20T12:00:00Z")

	// One payload with an unmapped key, then enough clean ones to fill and
	// overflow the ring several times over.
	RecordPlanRawPayload(dir, ProviderClaude, base, 200, t85Body)
	clean := `{"five_hour":{"utilization":%d},"seven_day":{"utilization":7}}`
	for i := 0; i < 3*PlanRawPayloadsPerProvider; i++ {
		RecordPlanRawPayload(dir, ProviderClaude,
			base.Add(time.Duration(i+1)*time.Minute), 200, fmt.Sprintf(clean, i))
	}

	got := ReadPlanRawPayloads(dir, ProviderClaude)
	if len(got) != PlanRawPayloadsPerProvider {
		t.Fatalf("store holds %d payloads, want the %d-deep ring",
			len(got), PlanRawPayloadsPerProvider)
	}
	found := false
	for _, e := range got {
		if e.Body == t85Body {
			found = true
		}
	}
	if !found {
		t.Fatalf("the payload carrying an unmapped key was evicted by %d clean ones",
			3*PlanRawPayloadsPerProvider)
	}
	// Oldest first, and the clean tail is the most recent traffic.
	if !got[len(got)-1].FetchedAt.After(got[0].FetchedAt) {
		t.Fatal("payloads are not ordered oldest first")
	}
}

// A refusal body is not a surface change. It is kept as evidence, but it
// must not claim novelty and evict the readings worth having.
func TestT84ARefusalBodyIsNotTreatedAsNovel(t *testing.T) {
	dir := t.TempDir()
	at := t85At(t, "2026-09-20T12:00:00Z")
	RecordPlanRawPayload(dir, ProviderClaude, at, 429, `{"type":"error","error":{"type":"rate_limit_error"}}`)
	got := ReadPlanRawPayloads(dir, ProviderClaude)
	if len(got) != 1 {
		t.Fatalf("the refusal body was not kept: %d payloads", len(got))
	}
	if len(got[0].UnmappedKeys) != 0 {
		t.Fatalf("a refusal claimed unmapped keys %v; it would outlive real readings", got[0].UnmappedKeys)
	}
}

// The unmapped set is read off the parser's own type, so a key the parser
// does not touch is named and a key it does is not.
func TestT84UnmappedKeysComeFromTheParsersOwnShape(t *testing.T) {
	got := unmappedPlanKeys(ProviderClaude, []byte(t85Body))
	if len(got) != 1 || got[0] != "seven_day_fable_five" {
		t.Fatalf("unmapped keys = %v, want just [seven_day_fable_five]", got)
	}

	// The real per-model surface travels inside limits[], which the parser
	// does read (🎯T86) — it must not be reported as novel.
	mapped := `{"five_hour":{"utilization":33,"resets_at":"2026-09-20T18:00:00Z"},` +
		`"limits":[{"kind":"weekly_scoped","percent":100,"resets_at":"2026-09-26T00:00:00Z",` +
		`"scope":{"model":{"id":"fable-5","display_name":"Fable"}}}]}`
	if got := unmappedPlanKeys(ProviderClaude, []byte(mapped)); len(got) != 0 {
		t.Fatalf("keys the parser reads were reported as unmapped: %v", got)
	}

	// A new field nested inside a window is named by its full path, not
	// swallowed by its mapped parent.
	nested := `{"five_hour":{"utilization":33,"overage_allowed":true}}`
	if got := unmappedPlanKeys(ProviderClaude, []byte(nested)); len(got) != 1 ||
		got[0] != "five_hour.overage_allowed" {
		t.Fatalf("nested unmapped key = %v, want [five_hour.overage_allowed]", got)
	}

	// Every provider that costs a request declares a shape, so none of
	// them can silently drop a surface change.
	for _, p := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor} {
		if _, ok := planParserShapes[p]; !ok {
			t.Fatalf("%s has no declared parser shape; its unmapped keys would never be seen", p)
		}
	}
}

// A body larger than the cap is truncated rather than stored whole: a
// vendor must not be able to grow this store by growing its response.
func TestT84StoredBodyIsCapped(t *testing.T) {
	dir := t.TempDir()
	huge := `{"x":"` + strings.Repeat("y", PlanRawBodyLimit*2) + `"}`
	RecordPlanRawPayload(dir, ProviderClaude, time.Now(), 200, huge)
	got := ReadPlanRawPayloads(dir, ProviderClaude)
	if len(got) != 1 {
		t.Fatalf("payloads = %d, want 1", len(got))
	}
	if len(got[0].Body) != PlanRawBodyLimit {
		t.Fatalf("stored body is %d bytes, want the %d cap", len(got[0].Body), PlanRawBodyLimit)
	}
}

// Nothing the store keeps is published. The snapshot every consumer reads
// carries the one body its own fetch produced, never the retained ring.
func TestT84StoreIsNotPublishedInTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		RecordPlanRawPayload(dir, ProviderClaude,
			t85At(t, "2026-09-20T12:00:00Z").Add(time.Duration(i)*time.Hour), 200, t85Body)
	}
	doc := planCacheSnapshot{FetchedAt: time.Now(), Backends: []PlanUsage{
		{Provider: ProviderClaude, Status: PlanUsageAvailable},
	}}
	path := filepath.Join(dir, planCacheSnapshotFile)
	if err := writePlanSnapshot(path, doc); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "seven_day_fable_five") {
		t.Fatal("the retained payload ring leaked into the published snapshot")
	}
	var back planCacheSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Backends) != 1 || back.Backends[0].RawBody != "" {
		t.Fatalf("snapshot republished a stored body: %+v", back.Backends)
	}
}

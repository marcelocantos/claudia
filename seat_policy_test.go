// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSeatPolicyStoreRoundTrips(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSeatPolicyStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := st.Get("nobody"); got.PreferProvider != "" || got.AllowedProviders != nil || got.AllowNone || len(got.ExcludeProviders) != 0 || got.HostMayInterrupt || got.HostNeverPark {
		t.Fatalf("zero value for unknown seat, got %+v", got)
	}
	policy := SeatPolicy{
		PreferProvider:   "claude",
		ExcludeProviders: []Provider{"cursor"},
		HostMayInterrupt:   true,
	}
	if err := st.Put("jv-x", policy); err != nil {
		t.Fatalf("put: %v", err)
	}

	reopened, err := OpenSeatPolicyStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Get("jv-x")
	if got.PreferProvider != "claude" || len(got.ExcludeProviders) != 1 || got.ExcludeProviders[0] != "cursor" || !got.HostMayInterrupt {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestSeatPolicyStoreAllowedSemantics(t *testing.T) {
	none := SeatPolicy{AllowNone: true}
	if providers, restricted := none.Allowed(); !restricted || len(providers) != 0 {
		t.Fatalf("AllowNone should be restricted-to-empty, got %v restricted=%v", providers, restricted)
	}
	unset := SeatPolicy{}
	if providers, restricted := unset.Allowed(); restricted || providers != nil {
		t.Fatalf("zero value should be unrestricted, got %v restricted=%v", providers, restricted)
	}
	some := SeatPolicy{AllowedProviders: []Provider{"claude", "grok"}}
	if providers, restricted := some.Allowed(); !restricted || len(providers) != 2 {
		t.Fatalf("explicit allow-list should be restricted, got %v restricted=%v", providers, restricted)
	}
}

func TestSeatPolicyStoreImportLegacyFile(t *testing.T) {
	legacyDir := t.TempDir()
	legacyPath := filepath.Join(legacyDir, "seatplan.json")
	legacy := map[string]SeatPolicy{
		"jv-old": {PreferProvider: "codex", HostMayInterrupt: true},
	}
	body, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(legacyPath, body, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	st, err := OpenSeatPolicyStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Pre-existing Claudia-side policy must not be overwritten by the import.
	if err := st.Put("jv-already-set", SeatPolicy{PreferProvider: "claude"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	legacy["jv-already-set"] = SeatPolicy{PreferProvider: "grok"}
	body, err = json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("remarshal legacy: %v", err)
	}
	if err := os.WriteFile(legacyPath, body, 0o600); err != nil {
		t.Fatalf("rewrite legacy: %v", err)
	}

	n, err := st.ImportLegacyFile(legacyPath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 imported entry, got %d", n)
	}
	if got := st.Get("jv-old"); got.PreferProvider != "codex" {
		t.Fatalf("imported entry mismatch: %+v", got)
	}
	if got := st.Get("jv-already-set"); got.PreferProvider != "claude" {
		t.Fatalf("existing Claudia policy was overwritten by import: %+v", got)
	}

	// A missing legacy file is not an error.
	n, err = st.ImportLegacyFile(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || n != 0 {
		t.Fatalf("missing legacy file should be a no-op, got n=%d err=%v", n, err)
	}
}

func TestResolveSeatPlacementForSeatReadsStoredPolicy(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSeatPolicyStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Put("jv-seat", SeatPolicy{ExcludeProviders: []Provider{"codex"}}); err != nil {
		t.Fatalf("put: %v", err)
	}

	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	zero := 0.0
	hundred := 100.0
	args := &SeatPlacementArgs{
		CurrentProvider: "claude",
		Now:             now,
		Usage: []PlanUsage{
			{Provider: "claude", Status: PlanUsageAvailable, FetchedAt: now, Windows: []PlanWindow{
				{Name: PlanWindowSession, RemainingPercent: &zero},
				{Name: PlanWindowWeekly, RemainingPercent: &zero},
			}},
			{Provider: "codex", Status: PlanUsageAvailable, FetchedAt: now, Windows: []PlanWindow{
				{Name: PlanWindowSession, RemainingPercent: &hundred},
				{Name: PlanWindowWeekly, RemainingPercent: &hundred},
			}},
			{Provider: "grok", Status: PlanUsageAvailable, FetchedAt: now, Windows: []PlanWindow{
				{Name: PlanWindowSession, RemainingPercent: &hundred},
				{Name: PlanWindowWeekly, RemainingPercent: &hundred},
			}},
		},
	}
	placement, err := ResolveSeatPlacementForSeat(context.Background(), "jv-seat", st, args)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if placement.Action == SeatMigrate && placement.Pick.Provider == "codex" {
		t.Fatalf("codex is excluded by the stored policy but was picked: %+v", placement)
	}
	// args itself (the caller's own copy) must be untouched.
	if args.ExcludeProviders != nil {
		t.Fatalf("ResolveSeatPlacementForSeat must not mutate the caller's args, got %+v", args.ExcludeProviders)
	}
}

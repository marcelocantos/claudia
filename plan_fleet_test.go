// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"testing"
	"time"
)

func TestFleetUsageListsFourProvidersInOrder(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	cursor, grok, claude, codex := 12.0, 81.5, 40.0, 0.0
	usage := []PlanUsage{
		{Provider: ProviderCodex, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &codex}}},
		{Provider: ProviderClaude, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &claude}}},
		{Provider: ProviderBedrock, Status: PlanUsageUnavailable, Reason: "no subscription window"},
		{Provider: ProviderGrok, Status: PlanUsageAvailable, PlanType: "SuperGrok", Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &grok}}},
		// Cursor's API bucket must not replace the billing-cycle total.
		{Provider: ProviderCursor, Status: PlanUsageAvailable, Windows: []PlanWindow{
			{Name: PlanWindowWeekly, RemainingPercent: &cursor},
			{Name: PlanWindowAPI, Model: "API", RemainingPercent: floatPtr(99)},
		}},
	}
	snap := ProjectFleetUsage(usage, now, now, nil)
	if len(snap.Providers) != 4 {
		t.Fatalf("providers = %d, want 4", len(snap.Providers))
	}
	want := []struct {
		p     Provider
		rem   float64
		admit bool
	}{
		{ProviderCursor, 12, true},
		{ProviderGrok, 81.5, true},
		{ProviderClaude, 40, true},
		{ProviderCodex, 0, false},
	}
	for i, w := range want {
		row := snap.Providers[i]
		if row.Provider != w.p {
			t.Errorf("providers[%d] = %s, want %s", i, row.Provider, w.p)
		}
		if row.RemainingPercent == nil || *row.RemainingPercent != w.rem {
			t.Errorf("%s remaining = %v, want %v", w.p, row.RemainingPercent, w.rem)
		}
		if row.Admit != w.admit {
			t.Errorf("%s admit = %v, want %v", w.p, row.Admit, w.admit)
		}
		if row.Window != PlanWindowWeekly {
			t.Errorf("%s window = %s, want weekly", w.p, row.Window)
		}
	}
	if snap.Providers[1].Reason != "" {
		t.Errorf("grok reason = %q", snap.Providers[1].Reason)
	}
}

func TestFleetUsageMissingProviderHasNoRemaining(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	snap := ProjectFleetUsage(nil, time.Time{}, now, nil)
	if len(snap.Providers) != 4 {
		t.Fatalf("providers = %d", len(snap.Providers))
	}
	for _, row := range snap.Providers {
		if row.Status != PlanUsageUnavailable || row.Reason != "no reading" {
			t.Errorf("%s status=%s reason=%q", row.Provider, row.Status, row.Reason)
		}
		if row.RemainingPercent != nil {
			t.Errorf("%s remaining = %v, want nil", row.Provider, row.RemainingPercent)
		}
		if !row.Admit {
			t.Errorf("%s with no reading should still admit", row.Provider)
		}
	}
}

func TestPickByRemainingSelectsFullestAdmitted(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	usage := fleetUsage(30, 90, 95, 20)
	// Claude's session is low, so its higher weekly figure cannot win.
	low := 5.0
	for i, u := range usage {
		if u.Provider == ProviderClaude {
			usage[i].Windows = append(usage[i].Windows, PlanWindow{Name: PlanWindowSession, RemainingPercent: &low})
		}
	}
	// Grok's weekly pool is spent. Cursor is the fullest that still admits.
	zero := 0.0
	for i, u := range usage {
		if u.Provider == ProviderGrok {
			usage[i].Windows = []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &zero}}
		}
	}
	pick, err := PickByRemaining(usage, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Provider != ProviderCursor || pick.RemainingPercent != 30 {
		t.Fatalf("pick = %+v, want cursor at 30", pick)
	}
}

func TestPickByRemainingTieBreaksByRosterOrder(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	usage := fleetUsage(50, 50, 50, 10)
	pick, err := PickByRemaining(usage, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Provider != ProviderCursor {
		t.Fatalf("tie pick = %s, want cursor", pick.Provider)
	}
}

func TestPickByRemainingNoneAdmittedIsExhausted(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	_, err := PickByRemaining(fleetUsage(0, 0, 0, 0), now, nil)
	if !errors.Is(err, ErrPlanExhausted) {
		t.Fatalf("err = %v", err)
	}
	_, err = PickByRemaining(nil, now, nil)
	if !errors.Is(err, ErrPlanExhausted) {
		t.Fatalf("no readings: %v", err)
	}
}

func TestPickByRemainingUsesSessionWhenWeeklyAbsent(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	codex := 70.0
	claude := 30.0
	usage := []PlanUsage{
		{Provider: ProviderCodex, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowSession, RemainingPercent: &codex}}},
		{Provider: ProviderClaude, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &claude}}},
	}
	pick, err := PickByRemaining(usage, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pick.Provider != ProviderCodex || pick.Window != PlanWindowSession {
		t.Fatalf("pick = %+v, want codex session", pick)
	}
}

func fleetUsage(cursor, grok, claude, codex float64) []PlanUsage {
	row := func(p Provider, rem float64) PlanUsage {
		return PlanUsage{
			Provider: p,
			Status:   PlanUsageAvailable,
			Windows:  []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: floatPtr(rem)}},
		}
	}
	return []PlanUsage{
		row(ProviderCursor, cursor),
		row(ProviderGrok, grok),
		row(ProviderClaude, claude),
		row(ProviderCodex, codex),
	}
}

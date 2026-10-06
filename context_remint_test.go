// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
	"time"
)

// 🎯T561 / 🎯T1013.4: ShouldRemintMigrate is the should-leave-only decision
// a context-blown seat's remint needs — ShouldVacate's own verdict, with an
// owner-ask escape that always wins regardless of the provider's pressure.

func remintUsage(rem, used float64, now time.Time) PlanUsage {
	week := now.Add(4 * 24 * time.Hour)
	return PlanUsage{
		Provider: ProviderClaude,
		Status:   PlanUsageAvailable,
		Windows: []PlanWindow{{
			Name: PlanWindowWeekly, RemainingPercent: floatPtr(rem), UsedPercent: floatPtr(used),
			ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
		}},
	}
}

func TestShouldRemintMigrateStaysWhenProviderHasHeadroom(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	u := remintUsage(60, 40, now) // weekly ok
	if ShouldRemintMigrate(u, false, now, nil) {
		t.Fatalf("weekly-ok, ownerAsked=false: want stay (false), got migrate")
	}
}

func TestShouldRemintMigrateOwnerAskedWinsOverHeadroom(t *testing.T) {
	// This is the exact scenario that regressed under claudia.ResolveSeatPlacement
	// in the T1013.4 scout prototype: a seat with plenty of weekly headroom and
	// no destination-provider reading at all. ShouldRemintMigrate must not care
	// about any destination — ownerAsked alone decides.
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	u := remintUsage(60, 40, now) // weekly ok — ShouldVacate alone would say stay
	if !ShouldRemintMigrate(u, true, now, nil) {
		t.Fatalf("weekly-ok, ownerAsked=true: want migrate (true), got stay")
	}
}

func TestShouldRemintMigrateExhaustedProviderMigratesWithoutOwnerAsk(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	u := remintUsage(0, 100, now) // weekly exhausted
	if !ShouldRemintMigrate(u, false, now, nil) {
		t.Fatalf("weekly exhausted, ownerAsked=false: want migrate (true), got stay")
	}
}

func TestShouldRemintMigrateUnknownReadingStays(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	u := PlanUsage{Provider: ProviderClaude, Status: PlanUsageUnavailable}
	if ShouldRemintMigrate(u, false, now, nil) {
		t.Fatalf("unavailable reading, ownerAsked=false: want stay (false), got migrate")
	}
	// Owner ask still wins even with no reading at all.
	if !ShouldRemintMigrate(u, true, now, nil) {
		t.Fatalf("unavailable reading, ownerAsked=true: want migrate (true), got stay")
	}
}

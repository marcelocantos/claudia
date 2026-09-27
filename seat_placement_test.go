// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
	"time"
)

func seatPlacementUsage(now time.Time) []PlanUsage {
	weekly := 7 * 24 * time.Hour
	return []PlanUsage{
		{Provider: ProviderGrok, Status: PlanUsageAvailable, FetchedAt: now,
			Windows: []PlanWindow{{Name: PlanWindowWeekly, UsedPercent: floatPtr(87), RemainingPercent: floatPtr(13),
				ResetsAt: timePtr(now.Add(weekly / 2)), LimitWindow: weekly}}},
		{Provider: ProviderCodex, Status: PlanUsageAvailable, FetchedAt: now,
			Windows: []PlanWindow{{Name: PlanWindowWeekly, UsedPercent: floatPtr(30), RemainingPercent: floatPtr(70),
				ResetsAt: timePtr(now.Add(weekly - time.Hour)), LimitWindow: weekly}}},
		{Provider: ProviderClaude, Status: PlanUsageAvailable, FetchedAt: now,
			Windows: []PlanWindow{{Name: PlanWindowWeekly, UsedPercent: floatPtr(93), RemainingPercent: floatPtr(7),
				ResetsAt: timePtr(now.Add(4 * time.Hour)), LimitWindow: weekly}}},
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestResolveSeatPlacementMovesSidecarGrokToClaude(t *testing.T) {
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	usage := seatPlacementUsage(now)
	args := &SeatPlacementArgs{CurrentProvider: "xai-oauth", Usage: usage,
		PreferProvider: ProviderGrok, Now: now, MaxUsageAge: 15 * time.Minute}
	got, err := ResolveSeatPlacement(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != SeatMigrate || got.From != ProviderGrok || got.Pick.Provider != ProviderClaude || got.Author != DecisionAuthor {
		t.Fatalf("hot Grok with ahead Codex and eligible Claude = %+v", got)
	}
	args.CurrentProvider = "anthropic"
	got, err = ResolveSeatPlacement(context.Background(), args)
	if err != nil || got.Action != SeatStay {
		t.Fatalf("second sweep must stay on Claude: %+v, %v", got, err)
	}
}

func TestResolveSeatPlacementSeparatesPreferenceFromProhibition(t *testing.T) {
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	args := &SeatPlacementArgs{CurrentProvider: "xai-oauth", Usage: seatPlacementUsage(now),
		PreferProvider: ProviderGrok, ExcludeProviders: []Provider{ProviderClaude}, Now: now}
	got, err := ResolveSeatPlacement(context.Background(), args)
	if err != nil || got.Action != SeatPark {
		t.Fatalf("explicit Claude prohibition with no eligible dest = %+v, %v", got, err)
	}
	args.ExcludeProviders = nil
	args.MaxUsageAge = time.Minute
	args.Usage[0].FetchedAt = now.Add(-2 * time.Minute)
	got, err = ResolveSeatPlacement(context.Background(), args)
	if err != nil || got.Action != SeatStay {
		t.Fatalf("stale source must not cause migration: %+v, %v", got, err)
	}
}

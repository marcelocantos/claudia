// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
	"time"
)

func TestResolveYTTShapedPrefersGrokWhenHealthy(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:           CapabilityTask,
		PreferPlan:     true,
		PreferProvider: ProviderGrok,
		Now:            now,
		Usage: []PlanUsage{
			{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "unpublished"},
			{Provider: ProviderClaude, Status: PlanUsageUnavailable, Reason: "unpublished"},
			{Provider: ProviderCodex, Status: PlanUsageUnavailable, Reason: "unpublished"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderGrok || got.Quality != ModelQualityStandard {
		t.Fatalf("pick %+v", got)
	}
}

func TestResolveSkipsHotClaude(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	hotClaude := PlanUsage{
		Provider: ProviderClaude,
		Status:   PlanUsageAvailable,
		Windows: []PlanWindow{{
			Name:             PlanWindowWeekly,
			RemainingPercent: floatPtr(20),
			UsedPercent:      floatPtr(80),
			ResetsAt:         &week,
			LimitWindow:      defaultWeeklyWindow,
		}},
	}
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:           CapabilityTask,
		PreferPlan:     true,
		PreferProvider: ProviderClaude,
		Now:            now,
		Usage: []PlanUsage{
			hotClaude,
			{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "unpublished"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderGrok {
		t.Fatalf("hot Claude must not win; got %+v", got)
	}
}

func TestResolveExcludeWalksNext(t *testing.T) {
	now := time.Now()
	first, err := Resolve(context.Background(), ModelPredicates{
		Mode:           CapabilityTask,
		PreferPlan:     true,
		PreferProvider: ProviderGrok,
		Now:            now,
		Usage:          []PlanUsage{{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(context.Background(), ModelPredicates{
		Mode:             CapabilityTask,
		PreferPlan:       true,
		ExcludeProviders: []Provider{first.Provider},
		Now:              now,
		Usage: []PlanUsage{
			{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "x"},
			{Provider: ProviderClaude, Status: PlanUsageUnavailable, Reason: "x"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Provider == first.Provider {
		t.Fatalf("exclude did not walk: first=%s second=%s", first.Provider, second.Provider)
	}
}

func TestResolveClaudeFirstWhenHeadroom(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	healthy := func(p Provider, rem, used float64) PlanUsage {
		return PlanUsage{
			Provider: p,
			Status:   PlanUsageAvailable,
			Windows: []PlanWindow{{
				Name:             PlanWindowWeekly,
				RemainingPercent: floatPtr(rem),
				UsedPercent:      floatPtr(used),
				ResetsAt:         &week,
				LimitWindow:      defaultWeeklyWindow,
			}},
		}
	}
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:           CapabilitySession,
		PreferPlan:     true,
		PreferProvider: ProviderClaude,
		Now:            now,
		Usage: []PlanUsage{
			healthy(ProviderClaude, 50, 50),
			healthy(ProviderGrok, 87, 13),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderClaude {
		t.Fatalf("claude-first with headroom: %+v", got)
	}
}

func TestResolveFailsClosed(t *testing.T) {
	_, err := Resolve(context.Background(), ModelPredicates{
		Mode:             CapabilityTask,
		PreferPlan:       true,
		ExcludeProviders: []Provider{ProviderClaude, ProviderGrok, ProviderCodex, ProviderCursor},
		Usage:            []PlanUsage{},
	})
	if err == nil {
		t.Fatal("expected fail closed")
	}
}

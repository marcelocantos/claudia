// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"strings"
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
			{
				Provider: ProviderGrok, Status: PlanUsageAvailable,
				Windows: []PlanWindow{{
					Name: PlanWindowWeekly, RemainingPercent: floatPtr(80), UsedPercent: floatPtr(20),
					ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderGrok {
		t.Fatalf("hot Claude must yield to slack, not catalog order; got %+v", got)
	}
}

func TestResolveExcludeDropsSlackWinner(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	weekly := func(p Provider, rem, used float64) PlanUsage {
		return PlanUsage{
			Provider: p, Status: PlanUsageAvailable,
			Windows: []PlanWindow{{
				Name: PlanWindowWeekly, RemainingPercent: floatPtr(rem), UsedPercent: floatPtr(used),
				ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
			}},
		}
	}
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode: CapabilityTask, Quality: ModelQualityStandard, PreferPlan: true, Now: now,
		ExcludeProviders: []Provider{ProviderCursor},
		Usage: []PlanUsage{
			weekly(ProviderCursor, 80, 20),
			weekly(ProviderClaude, 50, 50),
			weekly(ProviderGrok, 20, 80),
			{
				Provider: ProviderCodex, Status: PlanUsageAvailable,
				Windows: []PlanWindow{{
					Name: PlanWindowWeekly, RemainingPercent: floatPtr(0), UsedPercent: floatPtr(100),
					ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderClaude {
		t.Fatalf("excluding the slack winner must leave the next slack, got %+v", got)
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
			healthy(ProviderGrok, 50, 50),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderClaude {
		t.Fatalf("claude-first with headroom: %+v", got)
	}
}

func TestResolveCatalogPathPrefersPlanSlack(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	weekly := func(p Provider, rem, used float64) PlanUsage {
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
		Mode:       CapabilityTask,
		Quality:    ModelQualityStandard,
		PreferPlan: true,
		Now:        now,
		Usage: []PlanUsage{
			weekly(ProviderClaude, 50, 50),
			weekly(ProviderCursor, 80, 20),
			weekly(ProviderGrok, 20, 80),
			{
				Provider: ProviderCodex, Status: PlanUsageAvailable,
				Windows: []PlanWindow{{
					Name: PlanWindowWeekly, RemainingPercent: floatPtr(0), UsedPercent: floatPtr(100),
					ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderCursor || got.Model != "composer-2.5" {
		t.Fatalf("lone under/blue must beat ok Claude; got %+v", got)
	}
	if got.Band != PlanBandUnder {
		t.Fatalf("band %s", got.Band)
	}
}

func TestResolveStandardFailsClosedWhenSlackTied(t *testing.T) {
	_, err := Resolve(context.Background(), ModelPredicates{
		Mode:       CapabilityTask,
		Quality:    ModelQualityStandard,
		PreferPlan: true,
		Usage:      []PlanUsage{},
	})
	if err == nil {
		t.Fatal("equal slack must not pick by catalog order")
	}
	if !strings.Contains(err.Error(), "token-tied") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveStandardQualityExcludesOpusAndHaiku(t *testing.T) {
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:           CapabilityTask,
		Quality:        ModelQualityStandard,
		PreferPlan:     true,
		PreferProvider: ProviderClaude,
		Usage:          []PlanUsage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Quality != ModelQualityStandard || got.Model != "claude-sonnet-5" {
		t.Fatalf("standard+prefer claude: %+v", got)
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

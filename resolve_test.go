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

func TestResolveBackgroundAvoidsOverspentProviders(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	weekly := func(provider Provider, used float64) PlanUsage {
		return PlanUsage{Provider: provider, Status: PlanUsageAvailable,
			Windows: []PlanWindow{{Name: PlanWindowWeekly,
				UsedPercent: floatPtr(used), RemainingPercent: floatPtr(100 - used),
				ResetsAt: &week, LimitWindow: defaultWeeklyWindow}}}
	}
	if band := ClassifyPlan(weekly(ProviderGrok, 70), now, nil).Weekly; band != PlanBandAhead {
		t.Fatalf("test fixture must be orange, got %s", band)
	}
	intel := &ModelIntelArgs{Latest: []ModelObservation{
		{Generation: "grok-4.6", Effort: ModelEffortHigh, Purpose: ModelPurposeGeneral, Value: 80},
		{Generation: "claude-sonnet-5", Effort: ModelEffortHigh, Purpose: ModelPurposeGeneral, Value: 80},
	}}
	for _, path := range []struct {
		name  string
		intel *ModelIntelArgs
	}{
		{name: "catalog"},
		{name: "intel", intel: intel},
	} {
		t.Run(path.name, func(t *testing.T) {
			pred := ModelPredicates{
				Mode: CapabilityTask, Quality: ModelQualityStandard, PreferPlan: true,
				PreferProvider: ProviderGrok, Background: true, Now: now,
				ExcludeProviders: []Provider{ProviderCursor, ProviderCodex},
				Usage:            []PlanUsage{weekly(ProviderGrok, 70), weekly(ProviderClaude, 50)},
				Intel:            path.intel,
			}
			got, err := Resolve(context.Background(), pred)
			if err != nil || got.Provider != ProviderClaude {
				t.Fatalf("background must avoid preferred orange provider: %+v (%v)", got, err)
			}
			pred.Usage = []PlanUsage{weekly(ProviderGrok, 70), weekly(ProviderClaude, 80)}
			if _, err := Resolve(context.Background(), pred); err == nil {
				t.Fatal("background must refuse when only orange/red providers remain")
			}
			pred.Background = false
			got, err = Resolve(context.Background(), pred)
			if path.intel != nil && (err != nil || got.Provider != ProviderGrok) {
				t.Fatalf("interactive intel selection must preserve preferred orange provider: %+v (%v)", got, err)
			}
		})
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

func TestResolvePreferProviderBeatsSlack(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	healthy := func(p Provider, rem, used float64) PlanUsage {
		return PlanUsage{
			Provider: p, Status: PlanUsageAvailable,
			Windows: []PlanWindow{{
				Name: PlanWindowWeekly, RemainingPercent: floatPtr(rem), UsedPercent: floatPtr(used),
				ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
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
		t.Fatalf("prefer Claude with headroom must beat greener Grok: %+v", got)
	}
	if got.Author != DecisionAuthor {
		t.Fatalf("pick author = %q, want %q", got.Author, DecisionAuthor)
	}
}

func TestResolveRequireUsageRefusesWhenNoPublishedDest(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	_, err := Resolve(context.Background(), ModelPredicates{
		Mode:         CapabilitySession,
		PreferPlan:   true,
		RequireUsage: true,
		Now:          now,
		Usage: []PlanUsage{{
			Provider: ProviderGrok, Status: PlanUsageAvailable,
			Windows: []PlanWindow{{
				Name: PlanWindowWeekly, RemainingPercent: floatPtr(20), UsedPercent: floatPtr(80),
				ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
			}},
		}},
	})
	if err == nil {
		t.Fatal("require-usage with only a hot dest must refuse, not invent a catalog row")
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

func TestResolveBlueCursorBeatsUnpublishedCatalogFirst(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:       CapabilityTask,
		Quality:    ModelQualityStandard,
		PreferPlan: true,
		Now:        now,
		Usage: []PlanUsage{
			{Provider: ProviderClaude, Status: PlanUsageUnavailable, Reason: "unpublished"},
			{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "unpublished"},
			{Provider: ProviderCodex, Status: PlanUsageUnavailable, Reason: "unpublished"},
			{
				Provider: ProviderCursor,
				Status:   PlanUsageAvailable,
				Windows: []PlanWindow{{
					Name:             PlanWindowWeekly,
					RemainingPercent: floatPtr(80),
					UsedPercent:      floatPtr(20),
					ResetsAt:         &week,
					LimitWindow:      defaultWeeklyWindow,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderCursor || got.Model != "composer-2.5" {
		t.Fatalf("blue Cursor must beat unpublished catalog-first Claude; got %+v", got)
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

// 🎯T693 (jevons): Claude weekly under (71% used, 17h left) vs Grok weekly
// ok (5% used, 137h left) → Claude, even though Grok's raw pressure is
// more negative. RequireUsage, no PreferProvider — dest ranking, not
// Claude-first.
func TestResolveUnderBeatsOkDespiteGrokSlack(t *testing.T) {
	now := time.Date(2026, 9, 20, 20, 0, 0, 0, time.UTC)
	claudeReset := now.Add(17 * time.Hour)
	grokReset := now.Add(137 * time.Hour)
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:         CapabilitySession,
		PreferPlan:   true,
		RequireUsage: true,
		Now:          now,
		Usage: []PlanUsage{
			{
				Provider: ProviderClaude, Status: PlanUsageAvailable,
				Windows: []PlanWindow{{
					Name: PlanWindowWeekly, RemainingPercent: floatPtr(29), UsedPercent: floatPtr(71),
					ResetsAt: &claudeReset, LimitWindow: defaultWeeklyWindow,
				}},
			},
			{
				Provider: ProviderGrok, Status: PlanUsageAvailable,
				Windows: []PlanWindow{{
					Name: PlanWindowWeekly, RemainingPercent: floatPtr(95), UsedPercent: floatPtr(5),
					ResetsAt: &grokReset, LimitWindow: defaultWeeklyWindow,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderClaude {
		t.Fatalf("under must beat ok: got %+v", got)
	}
	if got.Band != PlanBandUnder {
		t.Fatalf("Claude band=%s, want under", got.Band)
	}
}

func TestResolveFableSpentDoesNotVetoClaude(t *testing.T) {
	now := time.Date(2026, 9, 20, 20, 0, 0, 0, time.UTC)
	claudeReset := now.Add(17 * time.Hour)
	grokReset := now.Add(137 * time.Hour)
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:         CapabilitySession,
		PreferPlan:   true,
		RequireUsage: true,
		Now:          now,
		Usage: []PlanUsage{
			{
				Provider: ProviderClaude, Status: PlanUsageAvailable,
				Windows: []PlanWindow{
					{
						Name: PlanWindowModelWeekly, Model: "Fable",
						RemainingPercent: floatPtr(0), UsedPercent: floatPtr(100),
						ResetsAt: &claudeReset, LimitWindow: defaultWeeklyWindow,
					},
					{
						Name: PlanWindowWeekly, RemainingPercent: floatPtr(29), UsedPercent: floatPtr(71),
						ResetsAt: &claudeReset, LimitWindow: defaultWeeklyWindow,
					},
				},
			},
			{
				Provider: ProviderGrok, Status: PlanUsageAvailable,
				Windows: []PlanWindow{{
					Name: PlanWindowWeekly, RemainingPercent: floatPtr(95), UsedPercent: floatPtr(5),
					ResetsAt: &grokReset, LimitWindow: defaultWeeklyWindow,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderClaude {
		t.Fatalf("Fable spent ≠ Claude unavailable: got %+v", got)
	}
	if got.Model == "claude-fable-5" {
		t.Fatalf("landed on spent Fable: %+v", got)
	}
}

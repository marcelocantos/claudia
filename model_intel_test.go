// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseModelSlugKeepsGenerationAndEffortApart(t *testing.T) {
	cases := []struct {
		in  string
		gen string
		eff ModelEffort
	}{
		{"claude-opus-5-max", "claude-opus-5", ModelEffortMax},
		{"claude-opus-4-6-high", "claude-opus-4-6", ModelEffortHigh},
		{"Claude Opus 4.6 (High)", "claude-opus-4.6", ModelEffortHigh},
		{"gpt-5.6-sol-xhigh", "gpt-5.6-sol", ModelEffortXHigh},
		{"grok-4.6", "grok-4.6", ModelEffortUnspecified},
		{"composer-2.5", "composer-2.5", ModelEffortUnspecified},
	}
	for _, tc := range cases {
		g, e := ParseModelSlug(tc.in)
		if g != tc.gen || e != tc.eff {
			t.Errorf("%q: got %s/%s want %s/%s", tc.in, g, e, tc.gen, tc.eff)
		}
	}
}

func TestRefreshModelIntelAppendsNotOverwrites(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("testdata", "modelintel", "aa_free.json"))
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	args := func(now time.Time) *ModelIntelArgs {
		return &ModelIntelArgs{Dir: dir, Now: now, FetchAA: func() ([]byte, error) { return raw, nil }}
	}
	run, err := RefreshModelIntel(context.Background(), args(t1))
	if err != nil || !run.OK || run.Count == 0 {
		t.Fatalf("first ingest: %+v %v", run, err)
	}
	run, err = RefreshModelIntel(context.Background(), args(t2))
	if err != nil || !run.OK {
		t.Fatalf("second ingest: %+v %v", run, err)
	}
	hist, err := ModelIntelHistory(args(t2), "claude-opus-5", ModelEffortHigh, ModelPurposeCoding)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("history len=%d want 2 (append, not overwrite): %+v", len(hist), hist)
	}
	if !hist[0].ObservedAt.Equal(t1) || !hist[1].ObservedAt.Equal(t2) {
		t.Fatalf("times %v %v", hist[0].ObservedAt, hist[1].ObservedAt)
	}
	latest, err := LatestModelIntel(args(t2))
	if err != nil {
		t.Fatal(err)
	}
	var codingHigh int
	for _, o := range latest {
		if o.Purpose == ModelPurposeCoding && o.Generation == "claude-opus-5" && o.Effort == ModelEffortHigh {
			codingHigh++
		}
	}
	if codingHigh != 1 {
		t.Fatalf("latest should keep one coding/opus/high, got %d", codingHigh)
	}
}

func TestRefreshModelIntelMissingKeySkips(t *testing.T) {
	t.Setenv(modelIntelEnvAAKey, "")
	dir := t.TempDir()
	run, err := RefreshModelIntel(context.Background(), &ModelIntelArgs{Dir: dir})
	if err == nil {
		t.Fatal("expected missing-key error")
	}
	if run.OK || run.Error == "" {
		t.Fatalf("run %+v", run)
	}
	obs, err := readObservations(dir)
	if err != nil || len(obs) != 0 {
		t.Fatalf("invented observations: %d %v", len(obs), err)
	}
}

func TestDriftModelIntelFlagsMoveAndBoardEvent(t *testing.T) {
	t1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	series := []ModelObservation{
		{ObservedAt: t1, Source: "aa", SourceRev: "4.2", Generation: "claude-opus-5", Effort: ModelEffortHigh, Purpose: ModelPurposeCoding, Value: 70},
		{ObservedAt: t1.Add(24 * time.Hour), Source: "aa", SourceRev: "4.2", Generation: "claude-opus-5", Effort: ModelEffortHigh, Purpose: ModelPurposeCoding, Value: 74},
		{ObservedAt: t1, Source: "aa", SourceRev: "4.2", Generation: "grok-4.6", Effort: ModelEffortHigh, Purpose: ModelPurposeCoding, Value: 60},
		{ObservedAt: t1.Add(24 * time.Hour), Source: "aa", SourceRev: "4.3", Generation: "grok-4.6", Effort: ModelEffortHigh, Purpose: ModelPurposeCoding, Value: 80},
	}
	drift, err := DriftModelIntel(&ModelIntelArgs{Latest: series})
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 2 {
		t.Fatalf("drift %d", len(drift))
	}
	var opus, grok *ModelIntelDrift
	for i := range drift {
		switch drift[i].Generation {
		case "claude-opus-5":
			opus = &drift[i]
		case "grok-4.6":
			grok = &drift[i]
		}
	}
	if opus == nil || !opus.Significant || opus.BoardEvent || opus.Delta != 4 {
		t.Fatalf("opus move: %+v", opus)
	}
	if grok == nil || !grok.BoardEvent || grok.Significant {
		t.Fatalf("grok board event should not count as a model move: %+v", grok)
	}
}

func aaIntel(t *testing.T) *ModelIntelArgs {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "modelintel", "aa_free.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	args := &ModelIntelArgs{Dir: dir, Now: now, FetchAA: func() ([]byte, error) { return raw, nil }}
	if _, err := RefreshModelIntel(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	return args
}

func TestResolvePurposeQualityPicksCheapestEffortAndGeneration(t *testing.T) {
	intel := aaIntel(t)
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode:       CapabilityTask,
		Purpose:    ModelPurposeCoding,
		Quality:    ModelQualityStandard,
		PreferPlan: true,
		Intel:      intel,
		Usage:      []PlanUsage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderGrok || got.Model != "grok-4.6" || got.Effort != ModelEffortHigh {
		t.Fatalf("want cheapest standard coding grok-4.6/high, got %+v", got)
	}
	if got.Purpose != ModelPurposeCoding || got.CostUSD != 0.1 {
		t.Fatalf("pick metadata %+v", got)
	}
}

func TestResolveRedYieldsToSlackThenCost(t *testing.T) {
	intel := aaIntel(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	hot := PlanUsage{
		Provider: ProviderGrok, Status: PlanUsageAvailable,
		Windows: []PlanWindow{{
			Name: PlanWindowWeekly, RemainingPercent: floatPtr(20), UsedPercent: floatPtr(80),
			ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
		}},
	}
	ok := PlanUsage{
		Provider: ProviderClaude, Status: PlanUsageAvailable,
		Windows: []PlanWindow{{
			Name: PlanWindowWeekly, RemainingPercent: floatPtr(50), UsedPercent: floatPtr(50),
			ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
		}},
	}
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode: CapabilityTask, Purpose: ModelPurposeCoding, Quality: ModelQualityStandard,
		PreferPlan: true, Intel: intel, Now: now, Usage: []PlanUsage{hot, ok},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderClaude || got.Model != "claude-sonnet-5" || got.Effort != ModelEffortHigh {
		t.Fatalf("hot grok must yield; cheapest remaining standard is sonnet high: %+v", got)
	}
}

func TestResolvePinMissesFloorFailsClosed(t *testing.T) {
	intel := aaIntel(t)
	_, err := Resolve(context.Background(), ModelPredicates{
		Mode: CapabilityTask, Purpose: ModelPurposeCoding, Quality: ModelQualityFrontier,
		Model: "claude-haiku-4-5", Effort: ModelEffortUnspecified,
		PreferPlan: true, Intel: intel, Usage: []PlanUsage{},
	})
	// haiku unspecified effort exists; frontier floor should exclude it.
	// Pin model without effort still applies the floor.
	if err == nil {
		t.Fatal("haiku must not meet frontier coding")
	}
	_, err = Resolve(context.Background(), ModelPredicates{
		Mode: CapabilityTask, Purpose: ModelPurposeCoding, Quality: ModelQualityFrontier,
		Model: "claude-opus-5", Effort: ModelEffortHigh,
		PreferPlan: true, Intel: intel, Usage: []PlanUsage{},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveFallsBackToGeneralWhenPurposeSeriesMissing(t *testing.T) {
	intel := &ModelIntelArgs{Latest: []ModelObservation{
		{Generation: "grok-4.6", Effort: ModelEffortHigh, Purpose: ModelPurposeGeneral, Value: 80, CostUSD: 0.10},
		{Generation: "claude-sonnet-5", Effort: ModelEffortHigh, Purpose: ModelPurposeGeneral, Value: 70, CostUSD: 0.20},
	}}
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode: CapabilityTask, Purpose: ModelPurposeAnalysis, Quality: ModelQualityStandard,
		PreferPlan: true, Intel: intel, Usage: []PlanUsage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Purpose != ModelPurposeGeneral || got.Provider != ProviderGrok || got.Model != "grok-4.6" {
		t.Fatalf("want general fallback grok-4.6, got %+v", got)
	}
	if !strings.Contains(got.Reason, "purpose_fallback_from=analysis") {
		t.Fatalf("reason missing fallback: %s", got.Reason)
	}
}

func TestResolveDoesNotFallBackWhenPurposeSeriesExists(t *testing.T) {
	intel := &ModelIntelArgs{Latest: []ModelObservation{
		{Generation: "claude-sonnet-5", Effort: ModelEffortHigh, Purpose: ModelPurposeAnalysis, Value: 70, CostUSD: 0.20},
		{Generation: "grok-4.6", Effort: ModelEffortHigh, Purpose: ModelPurposeGeneral, Value: 80, CostUSD: 0.10},
	}}
	_, err := Resolve(context.Background(), ModelPredicates{
		Mode: CapabilityTask, Purpose: ModelPurposeAnalysis, Quality: ModelQualityStandard,
		PreferPlan: true, Intel: intel, Usage: []PlanUsage{{
			Provider: ProviderClaude, Status: PlanUsageAvailable, Reason: "429 rate limited",
		}},
	})
	if err == nil {
		t.Fatal("analysis series exists; exhausted analysis must fail closed, not yield to general")
	}
}

func TestResolveCatalogPathUnchangedWithoutPurpose(t *testing.T) {
	got, err := Resolve(context.Background(), ModelPredicates{
		Mode: CapabilityTask, Quality: ModelQualityStandard, PreferPlan: true, Usage: []PlanUsage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Quality != ModelQualityStandard || got.Purpose != "" {
		t.Fatalf("catalog path leaked intel fields: %+v", got)
	}
}

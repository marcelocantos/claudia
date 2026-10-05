// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A missing purpose series may yield to general. An existing series does
// not become "missing" merely because its only model ran out of tokens.
func TestResolveAnalysisMissingVersusModelExhausted(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	general := ModelObservation{
		Generation: "grok-4.6", Effort: ModelEffortHigh,
		Purpose: ModelPurposeGeneral, Value: 80, CostUSD: 0.10,
	}
	analysis := ModelObservation{
		Generation: "claude-sonnet-5", Effort: ModelEffortHigh,
		Purpose: ModelPurposeAnalysis, Value: 70, CostUSD: 0.20,
	}
	// Both plans have headroom. Only Sonnet's separately metered model
	// window runs dry; Grok remains an eligible general alternative.
	claude := PlanUsage{Provider: ProviderClaude, Status: PlanUsageAvailable, Windows: []PlanWindow{
		{Name: PlanWindowWeekly, RemainingPercent: floatPtr(50), UsedPercent: floatPtr(50)},
		{Name: PlanWindowModelWeekly, Model: "Sonnet", RemainingPercent: floatPtr(0), UsedPercent: floatPtr(100)},
	}}
	grok := PlanUsage{Provider: ProviderGrok, Status: PlanUsageAvailable, Windows: []PlanWindow{
		{Name: PlanWindowWeekly, RemainingPercent: floatPtr(50), UsedPercent: floatPtr(50)},
	}}
	if !HasAvailableTokens(claude, now, nil) || ModelHasAvailableTokens(claude, "claude-sonnet-5", now, nil) || !ModelHasAvailableTokens(grok, "grok-4.6", now, nil) {
		t.Fatal("fixture must have a healthy Claude plan, spent Sonnet, and eligible Grok")
	}
	pred := ModelPredicates{
		Mode: CapabilityTask, Purpose: ModelPurposeAnalysis, Quality: ModelQualityStandard,
		Effort: ModelEffortHigh, PreferPlan: true, RequireUsage: true, Now: now,
		Usage: []PlanUsage{claude, grok},
	}
	// The explicit high effort removes unscored shelf substitutes. The
	// published usage requirement removes providers without a fixture.
	pred.Intel = &ModelIntelArgs{Latest: []ModelObservation{general}}
	missing, err := Resolve(context.Background(), pred)
	if err != nil {
		t.Fatalf("absent analysis series should yield to eligible general: %v", err)
	}
	if missing.Provider != ProviderGrok || missing.Model != "grok-4.6" || missing.Purpose != ModelPurposeGeneral ||
		!strings.Contains(missing.Reason, "purpose_fallback_from=analysis") {
		t.Fatalf("absent series fallback: %+v", missing)
	}

	pred.Intel = &ModelIntelArgs{Latest: []ModelObservation{analysis, general}}
	// Confirm this exact fixture would select the analysis row before
	// exhausting the model; a failure below must be token-driven.
	available := claude
	available.Windows = append([]PlanWindow(nil), claude.Windows...)
	available.Windows[1].RemainingPercent = floatPtr(40)
	available.Windows[1].UsedPercent = floatPtr(60)
	pred.Usage = []PlanUsage{available, grok}
	pick, err := Resolve(context.Background(), pred)
	if err != nil || pick.Provider != ProviderClaude || pick.Model != "claude-sonnet-5" || pick.Purpose != ModelPurposeAnalysis || strings.Contains(pick.Reason, "purpose_fallback_from=") {
		t.Fatalf("eligible analysis control: pick=%+v err=%v", pick, err)
	}

	pred.Usage = []PlanUsage{claude, grok}
	// Prove the general candidate stays independently selectable in the
	// exhausted snapshot; this rules out an error caused by all providers
	// being unavailable rather than by the fail-closed purpose boundary.
	generalPred := pred
	generalPred.Purpose = ModelPurposeGeneral
	generalPick, err := Resolve(context.Background(), generalPred)
	if err != nil || generalPick.Provider != ProviderGrok || generalPick.Model != "grok-4.6" {
		t.Fatalf("general alternative control: pick=%+v err=%v", generalPick, err)
	}
	if got, err := Resolve(context.Background(), pred); err == nil {
		t.Fatalf("spent analysis series must fail closed, not yield to general: %+v", got)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"math"
	"testing"
	"time"
)

func TestClassifyWindowJevonsOracles(t *testing.T) {
	th := DefaultPlanThresholds()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour) // 50% of 7d remaining
	weekly := func(rem, used float64) PlanWindow {
		return PlanWindow{
			Name:             PlanWindowWeekly,
			RemainingPercent: floatPtr(rem),
			UsedPercent:      floatPtr(used),
			ResetsAt:         &week,
			LimitWindow:      defaultWeeklyWindow,
		}
	}

	if got := ClassifyWindow(weekly(20, 80), now, th); got != PlanBandHot {
		t.Fatalf("80/50: band=%s want hot", got)
	}
	if got := ClassifyWindow(weekly(30, 70), now, th); got != PlanBandAhead {
		t.Fatalf("70/50: band=%s want ahead", got)
	}
	if got := ClassifyWindow(weekly(50, 50), now, th); got != PlanBandOK {
		t.Fatalf("50/50: band=%s want ok", got)
	}
	if got := ClassifyWindow(weekly(0, 100), now, th); got != PlanBandExhausted {
		t.Fatalf("0 remaining: band=%s want exhausted", got)
	}
}

func TestClassifyPlanExhaustedReasonAndUnpublished(t *testing.T) {
	now := time.Now()
	// A 429 from the usage meter is unpublished, not spent (jevons 🎯T677).
	ex := ClassifyPlan(PlanUsage{
		Provider: ProviderClaude,
		Status:   PlanUsageUnavailable,
		Reason:   `Claude usage HTTP 429: { "error": { "type": "rate_limit_error" } }`,
	}, now, nil)
	if ex.Weekly != PlanBandUnpublished || ex.ExhaustedReason || ex.Session != PlanSessionUnpublished {
		t.Fatalf("429-unavailable: %+v", ex)
	}

	unpub := ClassifyPlan(PlanUsage{
		Provider: ProviderGrok,
		Status:   PlanUsageUnavailable,
		Reason:   "no plan-remaining published",
	}, now, nil)
	if unpub.Weekly != PlanBandUnpublished || unpub.ExhaustedReason {
		t.Fatalf("unpublished: %+v", unpub)
	}
}

func TestHasAvailableTokensAutomaticBar(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	snap := func(weeklyRem, weeklyUsed float64, sess *float64) PlanUsage {
		u := PlanUsage{
			Provider:  ProviderClaude,
			Status:    PlanUsageAvailable,
			FetchedAt: now,
			Windows: []PlanWindow{{
				Name:             PlanWindowWeekly,
				RemainingPercent: floatPtr(weeklyRem),
				UsedPercent:      floatPtr(weeklyUsed),
				ResetsAt:         &week,
				LimitWindow:      defaultWeeklyWindow,
			}},
		}
		if sess != nil {
			u.Windows = append(u.Windows, PlanWindow{
				Name:             PlanWindowSession,
				RemainingPercent: sess,
			})
		}
		return u
	}

	if !HasAvailableTokens(snap(50, 50, floatPtr(80)), now, nil) {
		t.Fatal("ok weekly + healthy session should have tokens")
	}
	if HasAvailableTokens(snap(20, 80, floatPtr(80)), now, nil) {
		t.Fatal("hot weekly must fail the automatic token bar")
	}
	if HasAvailableTokens(snap(50, 50, floatPtr(0)), now, nil) {
		t.Fatal("session exhausted must fail")
	}
	if HasAvailableTokens(snap(50, 50, floatPtr(10)), now, nil) {
		t.Fatal("session low must fail")
	}
	if !HasAvailableTokens(PlanUsage{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "unpublished"}, now, nil) {
		t.Fatal("unpublished is not a veto")
	}
	if !HasAvailableTokens(PlanUsage{Provider: ProviderClaude, Status: PlanUsageUnavailable, Reason: "rate limited"}, now, nil) {
		t.Fatal("rate-limited meter is unpublished, not a veto")
	}
}

func TestShouldVacateHotNotUnpublished(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	week := now.Add(3*24*time.Hour + 12*time.Hour)
	hot := PlanUsage{
		Provider: ProviderClaude, Status: PlanUsageAvailable,
		Windows: []PlanWindow{{
			Name: PlanWindowWeekly, RemainingPercent: floatPtr(20), UsedPercent: floatPtr(80),
			ResetsAt: &week, LimitWindow: defaultWeeklyWindow,
		}},
	}
	if !ShouldVacate(hot, now, nil) {
		t.Fatal("hot weekly must vacate")
	}
	if ShouldVacate(PlanUsage{
		Provider: ProviderClaude, Status: PlanUsageUnavailable,
		Reason: "Claude usage HTTP 429: rate_limit_error",
	}, now, nil) {
		t.Fatal("429-unavailable must not vacate")
	}
}

func TestIsExhaustedReason(t *testing.T) {
	if !IsExhaustedReason(`Claude usage HTTP 429: { "error": { "type": "rate_limit_error" } }`) {
		t.Fatal("429 JSON")
	}
	if !IsExhaustedReason("Rate limited. Please try again later.") {
		t.Fatal("rate limited prose")
	}
	if IsExhaustedReason("SuperGrok publishes no plan-remaining API") {
		t.Fatal("unpublished must not look exhausted")
	}
	if IsExhaustedReason("") {
		t.Fatal("empty")
	}
}

func TestPressureSpentIsInf(t *testing.T) {
	if p := Pressure(100, 50, DefaultPlanThresholds()); !math.IsInf(p, 1) {
		t.Fatalf("spent window pressure = %v want +Inf", p)
	}
}

func TestT641WeekStartIsOK(t *testing.T) {
	th := DefaultPlanThresholds()
	if th.ShrinkPriorK != 100 || th.PanicAmberLn != 0.49 || th.PanicRedLn != 1 || th.WasteLockedLn != -1.5 {
		t.Fatalf("defaults moved: %+v", th)
	}
	now := time.Date(2026, 9, 12, 11, 26, 0, 0, time.UTC)
	week := func(rem, used float64, remTimePct float64) PlanWindow {
		resets := now.Add(time.Duration(remTimePct/100*float64(defaultWeeklyWindow)) * time.Second)
		return PlanWindow{
			Name:             PlanWindowWeekly,
			RemainingPercent: floatPtr(rem),
			UsedPercent:      floatPtr(used),
			ResetsAt:         &resets,
			LimitWindow:      defaultWeeklyWindow,
		}
	}
	if got := ClassifyWindow(week(88, 12, 98.06), now, th); got != PlanBandOK {
		t.Fatalf("Codex week-start 12/1.94 → %s, want ok", got)
	}
	if got := ClassifyWindow(week(85, 15, 94.32), now, th); got != PlanBandOK {
		t.Fatalf("Grok week-start 15/5.68 → %s, want ok", got)
	}
}

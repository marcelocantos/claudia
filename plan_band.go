// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"math"
	"strings"
	"time"
)

// PlanBand is the library verdict for one plan window (🎯T61.1).
// Names match the Jevons T596 rubric so hosts can paint without re-deriving.
type PlanBand string

const (
	PlanBandOK          PlanBand = "ok"
	PlanBandAhead       PlanBand = "ahead"
	PlanBandHot         PlanBand = "hot"
	PlanBandUnder       PlanBand = "under"
	PlanBandLocked      PlanBand = "locked"
	PlanBandExhausted   PlanBand = "exhausted"
	PlanBandUnpublished PlanBand = "unpublished"
)

// PlanSessionStatus is session-window eligibility (not leftover-vs-time).
type PlanSessionStatus string

const (
	PlanSessionOK          PlanSessionStatus = "ok"
	PlanSessionLow         PlanSessionStatus = "low"
	PlanSessionExhausted   PlanSessionStatus = "exhausted"
	PlanSessionUnpublished PlanSessionStatus = "unpublished"
)

// PlanThresholds are the pressure-model vertices (🎯T61.1).
// Zero values mean "use DefaultPlanThresholds for that field".
type PlanThresholds struct {
	WarmupElapsedPercent     float64
	EarlyAlarmUsedPercent    float64
	LowRemainingPercent      float64
	CriticalRemainingPercent float64
	DampLambdaPercent        float64
	AheadMarginPercent       float64
	ShrinkPriorK             float64
	PanicAmberLn             float64
	PanicRedLn               float64
	WasteUnderLn             float64
	WasteLockedLn            float64
}

// DefaultPlanThresholds matches the owner-tuned Spend Pressure Map (🎯T641).
func DefaultPlanThresholds() PlanThresholds {
	return PlanThresholds{
		WarmupElapsedPercent:     5,
		EarlyAlarmUsedPercent:    25,
		LowRemainingPercent:      15,
		CriticalRemainingPercent: 5,
		DampLambdaPercent:        5,
		AheadMarginPercent:       2,
		ShrinkPriorK:             100,
		PanicAmberLn:             0.49,
		PanicRedLn:               1.00,
		WasteUnderLn:             -0.60,
		WasteLockedLn:            -1.50,
	}
}

func (th PlanThresholds) withDefaults() PlanThresholds {
	d := DefaultPlanThresholds()
	if th.WarmupElapsedPercent == 0 {
		th.WarmupElapsedPercent = d.WarmupElapsedPercent
	}
	if th.EarlyAlarmUsedPercent == 0 {
		th.EarlyAlarmUsedPercent = d.EarlyAlarmUsedPercent
	}
	if th.LowRemainingPercent == 0 {
		th.LowRemainingPercent = d.LowRemainingPercent
	}
	if th.CriticalRemainingPercent == 0 {
		th.CriticalRemainingPercent = d.CriticalRemainingPercent
	}
	if th.DampLambdaPercent == 0 {
		th.DampLambdaPercent = d.DampLambdaPercent
	}
	if th.AheadMarginPercent == 0 {
		th.AheadMarginPercent = d.AheadMarginPercent
	}
	if th.ShrinkPriorK == 0 {
		th.ShrinkPriorK = d.ShrinkPriorK
	}
	if th.PanicAmberLn == 0 {
		th.PanicAmberLn = d.PanicAmberLn
	}
	if th.PanicRedLn == 0 {
		th.PanicRedLn = d.PanicRedLn
	}
	if th.WasteUnderLn == 0 {
		th.WasteUnderLn = d.WasteUnderLn
	}
	if th.WasteLockedLn == 0 {
		th.WasteLockedLn = d.WasteLockedLn
	}
	return th
}

// PlanVerdict is ClassifyPlan's result for one provider snapshot.
type PlanVerdict struct {
	Usage           PlanUsage
	Weekly          PlanBand
	Session         PlanSessionStatus
	ExhaustedReason bool
}

// ClassifyPlan attaches weekly and session verdicts to one PlanUsage.
// th nil uses DefaultPlanThresholds. now zero uses time.Now.
func ClassifyPlan(usage PlanUsage, now time.Time, th *PlanThresholds) PlanVerdict {
	if now.IsZero() {
		now = time.Now()
	}
	thresholds := DefaultPlanThresholds()
	if th != nil {
		thresholds = th.withDefaults()
	}
	v := PlanVerdict{Usage: usage}
	// Unavailable is unpublished, including a 429 from the usage meter
	// (jevons 🎯T677). Only a number the provider published can empty a
	// band; a failed reading is unknown, not spent.
	if usage.Status != PlanUsageAvailable {
		v.Weekly = PlanBandUnpublished
		v.Session = PlanSessionUnpublished
		return v
	}
	if IsExhaustedReason(usage.Reason) {
		v.ExhaustedReason = true
	}
	if w, ok := primaryAllowanceWindow(usage); ok {
		v.Weekly = ClassifyWindow(w, now, thresholds)
	} else {
		v.Weekly = PlanBandUnpublished
	}
	if w, ok := windowNamed(usage, PlanWindowSession); ok {
		v.Session = classifySession(w, thresholds)
	} else {
		v.Session = PlanSessionUnpublished
	}
	return v
}

// ClassifyWindow classifies one window at now.
func ClassifyWindow(w PlanWindow, now time.Time, th PlanThresholds) PlanBand {
	th = th.withDefaults()
	if w.RemainingPercent != nil && *w.RemainingPercent <= 0 {
		return PlanBandExhausted
	}
	used := usedPercent(w)
	rtp, hasTime := remainingTimePercent(w, now)
	if !hasTime || used == nil {
		return PlanBandOK
	}
	elapsed := 100 - rtp
	return BandOfPressure(Pressure(*used, elapsed, th), th)
}

// HasAvailableTokens reports whether this snapshot still has usable plan
// capacity. Unpublished / unknown is not a veto — only known exhaustion,
// weekly-hot, and session-low/exhausted fail. This is the automatic
// Resolve predicate (🎯T61.3).
func HasAvailableTokens(usage PlanUsage, now time.Time, th *PlanThresholds) bool {
	v := ClassifyPlan(usage, now, th)
	switch v.Session {
	case PlanSessionLow, PlanSessionExhausted:
		return false
	}
	switch v.Weekly {
	case PlanBandHot, PlanBandExhausted:
		return false
	}
	return true
}

// ShouldVacate reports that running seats on this provider must leave
// (weekly hot/exhausted, or session 0%). Session remaining-low and an
// unreadable (unpublished) meter do not bounce the fleet.
func ShouldVacate(usage PlanUsage, now time.Time, th *PlanThresholds) bool {
	v := ClassifyPlan(usage, now, th)
	if v.Session == PlanSessionExhausted {
		return true
	}
	switch v.Weekly {
	case PlanBandHot, PlanBandExhausted:
		return true
	default:
		return false
	}
}

// IsExhaustedReason reports a 429 / rate-limit reason (allowance gone),
// not "this backend publishes no remaining number".
func IsExhaustedReason(reason string) bool {
	s := strings.ToLower(reason)
	if s == "" {
		return false
	}
	return strings.Contains(s, "429") ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "rate-limit") ||
		strings.Contains(s, "rate limited")
}

// Pressure is ln(current/required) for one window (🎯T596 / 🎯T61.1).
func Pressure(used, elapsed float64, th PlanThresholds) float64 {
	th = th.withDefaults()
	remaining := 100 - used
	timeLeft := 100 - elapsed
	if timeLeft <= 0 {
		timeLeft = 0.0001
	}
	if remaining <= 0 {
		return math.Inf(1)
	}
	k := th.ShrinkPriorK
	lambda := k * (timeLeft / 100)
	current := (used + lambda) / (elapsed + lambda)
	required := remaining / timeLeft
	return math.Log(current / required)
}

// BandOfPressure maps pressure onto owner-visible bands.
func BandOfPressure(p float64, th PlanThresholds) PlanBand {
	th = th.withDefaults()
	switch {
	case p >= th.PanicRedLn:
		return PlanBandHot
	case p >= th.PanicAmberLn:
		return PlanBandAhead
	case p <= th.WasteLockedLn:
		return PlanBandLocked
	case p <= th.WasteUnderLn:
		return PlanBandUnder
	default:
		return PlanBandOK
	}
}

func classifySession(w PlanWindow, th PlanThresholds) PlanSessionStatus {
	if w.RemainingPercent == nil {
		return PlanSessionUnpublished
	}
	if *w.RemainingPercent <= 0 {
		return PlanSessionExhausted
	}
	if *w.RemainingPercent <= th.LowRemainingPercent {
		return PlanSessionLow
	}
	return PlanSessionOK
}

func primaryAllowanceWindow(u PlanUsage) (PlanWindow, bool) {
	if w, ok := planLevelWindow(u, PlanWindowWeekly); ok {
		return w, true
	}
	for _, w := range u.Windows {
		if w.Name == PlanWindowSession || w.Name == PlanWindowModelWeekly || w.Name == "" {
			continue
		}
		if strings.TrimSpace(w.Model) != "" {
			continue
		}
		return w, true
	}
	return PlanWindow{}, false
}

func planLevelWindow(u PlanUsage, name PlanWindowName) (PlanWindow, bool) {
	for _, w := range u.Windows {
		if w.Name != name || strings.TrimSpace(w.Model) != "" {
			continue
		}
		return w, true
	}
	return PlanWindow{}, false
}

func windowNamed(u PlanUsage, name PlanWindowName) (PlanWindow, bool) {
	for _, w := range u.Windows {
		if w.Name == name {
			return w, true
		}
	}
	return PlanWindow{}, false
}

func usedPercent(w PlanWindow) *float64 {
	if w.UsedPercent != nil {
		return w.UsedPercent
	}
	if w.RemainingPercent != nil {
		u := 100 - *w.RemainingPercent
		return &u
	}
	return nil
}

const (
	defaultWeeklyWindow  = 7 * 24 * time.Hour
	defaultSessionWindow = 5 * time.Hour
	defaultMonthlyWindow = 30 * 24 * time.Hour
)

func remainingTimePercent(w PlanWindow, now time.Time) (float64, bool) {
	if w.ResetsAt == nil {
		return 0, false
	}
	lim := w.LimitWindow
	if lim <= 0 {
		switch w.Name {
		case PlanWindowWeekly:
			lim = defaultWeeklyWindow
		case PlanWindowSession:
			lim = defaultSessionWindow
		default:
			if strings.EqualFold(string(w.Name), "monthly") {
				lim = defaultMonthlyWindow
			} else {
				return 0, false
			}
		}
	}
	pct := 100 * w.ResetsAt.Sub(now).Seconds() / lim.Seconds()
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

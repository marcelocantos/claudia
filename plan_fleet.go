// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"time"
)

// PickRemaining is the grant and task value that selects the fullest
// admitted provider among the fleet roster.
const PickRemaining = "remaining"

// fleetProviders is the pick-by-remaining roster, in stable order:
// Cursor, SuperGrok (Grok), Claude, Codex. That order is the usage
// listing and the tie-break: an earlier provider wins when the primary
// remaining percents are equal.
func fleetProviders() []Provider {
	return []Provider{ProviderCursor, ProviderGrok, ProviderClaude, ProviderCodex}
}

// FleetUsageRow is one provider in the stable usage roster. The four
// fleet providers are always present, in roster order. RemainingPercent
// is nil when the provider published no primary remaining number —
// never a fabricated percent.
type FleetUsageRow struct {
	Provider         Provider        `json:"provider"`
	Status           PlanUsageStatus `json:"status"`
	Admit            bool            `json:"admit"`
	RemainingPercent *float64        `json:"remaining_percent"`
	Window           PlanWindowName  `json:"window,omitempty"`
	Reason           string          `json:"reason,omitempty"`
}

// FleetUsageSnapshot is the stable plan-usage roster: Cursor, Grok
// (SuperGrok), Claude, and Codex, each with the primary remaining
// percent and the ADMIT predicate.
type FleetUsageSnapshot struct {
	FetchedAt time.Time       `json:"fetched_at,omitzero"`
	Providers []FleetUsageRow `json:"providers"`
}

// FleetPick is the provider [PickByRemaining] chose.
type FleetPick struct {
	Provider         Provider
	RemainingPercent float64
	Window           PlanWindowName
}

// ProjectFleetUsage projects usage onto the fleet roster. A provider
// absent from usage is a row with status unavailable, reason "no
// reading", and a nil remaining percent. Admit is [HasAvailableTokens]:
// an unpublished reading is not a veto, and a known exhaustion is not
// admitted.
func ProjectFleetUsage(usage []PlanUsage, fetchedAt, now time.Time, th *PlanThresholds) FleetUsageSnapshot {
	if now.IsZero() {
		now = time.Now()
	}
	by := indexPlanUsage(usage)
	rows := make([]FleetUsageRow, 0, 4)
	for _, p := range fleetProviders() {
		u, ok := by[p]
		if !ok {
			u = PlanUsage{Provider: p, Status: PlanUsageUnavailable, Reason: "no reading"}
		}
		row := FleetUsageRow{
			Provider: u.Provider,
			Status:   u.Status,
			Admit:    HasAvailableTokens(u, now, th),
			Reason:   u.Reason,
		}
		if u.Status == "" {
			row.Status = PlanUsageUnavailable
		}
		if pct, window, ok := fleetRemaining(u); ok {
			row.RemainingPercent = floatPtr(pct)
			row.Window = window
		}
		rows = append(rows, row)
	}
	return FleetUsageSnapshot{FetchedAt: fetchedAt, Providers: rows}
}

// PickByRemaining selects the fleet provider with the greatest primary
// remaining percent among rows [HasAvailableTokens] admits. The primary
// window is the weekly (or billing-cycle) allowance; a session percent
// is used only when no longer window was published. A row with no
// remaining percent cannot win. Ties break by roster order (cursor,
// grok, claude, codex). None admitted with a number is [ErrPlanExhausted].
func PickByRemaining(usage []PlanUsage, now time.Time, th *PlanThresholds) (FleetPick, error) {
	if now.IsZero() {
		now = time.Now()
	}
	by := indexPlanUsage(usage)
	var best *FleetPick
	for _, p := range fleetProviders() {
		u, ok := by[p]
		if !ok || !HasAvailableTokens(u, now, th) {
			continue
		}
		pct, window, ok := fleetRemaining(u)
		if !ok {
			continue
		}
		if best != nil && pct <= best.RemainingPercent {
			continue
		}
		best = &FleetPick{Provider: p, RemainingPercent: pct, Window: window}
	}
	if best == nil {
		return FleetPick{}, fmt.Errorf("%w: no admitted remaining percent among cursor, grok, claude, and codex", ErrPlanExhausted)
	}
	return *best, nil
}

func indexPlanUsage(usage []PlanUsage) map[Provider]PlanUsage {
	by := make(map[Provider]PlanUsage, len(usage))
	for _, u := range usage {
		by[u.Provider] = u
	}
	return by
}

// fleetRemaining is the primary allowance still left: the weekly or
// billing-cycle window, else the session window when that is all the
// provider published. Per-model windows are not the account remaining.
func fleetRemaining(u PlanUsage) (pct float64, window PlanWindowName, ok bool) {
	if u.Status != PlanUsageAvailable {
		return 0, "", false
	}
	if w, found := primaryAllowanceWindow(u); found && w.RemainingPercent != nil {
		return *w.RemainingPercent, w.Name, true
	}
	if w, found := planLevelWindow(u, PlanWindowSession); found && w.RemainingPercent != nil {
		return *w.RemainingPercent, w.Name, true
	}
	return 0, "", false
}

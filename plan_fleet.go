// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"strings"
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

// FleetReasonBinaryNotFound is [FleetUsageRow.Reason]'s prefix for a
// fleet provider whose CLI this host cannot resolve. Such a provider is
// not admitted and pick-by-remaining never chooses it: a plan with room
// is no use when the seat would fail at start with "executable not
// found" (Colossus, 2026-10-07: codex at 97% won every pick and every
// seat died).
const FleetReasonBinaryNotFound = "binary_not_found"

// ProviderBinaryError reports whether provider can start on this host.
// It is nil when the provider's backend on that surface runs no local
// CLI — session seats for Cursor and Grok run in the plan sidecar, and
// Bedrock and Ollama are API backends — or when the CLI resolves the way
// the backend itself will resolve it (env override, known install dirs,
// PATH). Otherwise it is the resolver's own "executable not found" error.
// session selects the Session (seat) surface; false is the Task surface.
func ProviderBinaryError(provider Provider, session bool) error {
	if session && useOMP(Config{Provider: provider}) {
		return nil
	}
	var err error
	switch provider {
	case "", ProviderClaude:
		_, err = resolveClaudeBin()
	case ProviderCodex:
		_, err = resolveCodexBin()
	case ProviderGrok:
		_, err = resolveGrokBin()
	case ProviderCursor:
		_, err = resolveCursorBin()
	}
	return err
}

// ExcludeUnlaunchable marks each row of snap whose provider is a key of
// missing as not admitted, with Reason "binary_not_found: <detail>". The
// map is what the daemon found it cannot launch (see
// [ProviderBinaryError]); the CLI that prints the roster runs in another
// process with another PATH, so it must not decide this itself.
func ExcludeUnlaunchable(snap FleetUsageSnapshot, missing map[Provider]string) FleetUsageSnapshot {
	for i, row := range snap.Providers {
		detail, ok := missing[row.Provider]
		if !ok {
			continue
		}
		snap.Providers[i].Admit = false
		snap.Providers[i].Reason = FleetReasonBinaryNotFound + ": " + strings.TrimSpace(detail)
	}
	return snap
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

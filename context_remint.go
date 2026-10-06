// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "time"

// ShouldRemintMigrate answers the narrow, destination-independent question
// a context-blown seat's remint decision needs (🎯T561 / 🎯T1013.4): should
// this seat leave its CURRENT provider, full stop — not where it should go.
//
// It is ShouldVacate's own verdict, with one escape: ownerAsked is true when
// the owner explicitly asked for a cross-provider move, and that always
// wins, even off a provider with plenty of headroom left. This mirrors the
// "owner asked always wins" contract jevons' contextRemintPlan has carried
// since 🎯T561; previously enforced by a local jevons branch ahead of ever
// calling the (already-thin) claudia.ShouldVacate delegate, now folded into
// this one call so no independent decision logic is left on the jevons side
// (🎯T1013.6).
//
// Deliberately NOT a destination search: the caller already has (or picks
// separately via Resolve/ResolveSeatPlacement) the provider to move to. A
// full stay/defer/park/migrate placement verdict needs a destination-
// provider plan reading this decision point never had — see
// ResolveSeatPlacement for that larger, different question.
func ShouldRemintMigrate(usage PlanUsage, ownerAsked bool, now time.Time, th *PlanThresholds) bool {
	if ownerAsked {
		return true
	}
	return ShouldVacate(usage, now, th)
}

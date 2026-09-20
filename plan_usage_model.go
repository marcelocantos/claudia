// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"strings"
	"time"
)

// 🎯T86: a plan with headroom can still hold a model with none.
//
// Plan windows are per account. Anthropic meters its premium model
// separately, so on 2026-09-20 the Claude plan read 67% weekly used —
// plenty left — while Fable itself was at 100%, and every seat the
// resolver placed on Fable was placed on a model that could not accept a
// turn. The provider-level check cannot see that, because it is looking
// at a different number.
//
// The matching is deliberately narrow. Per-model windows are labelled by
// the vendor ("Fable", "Opus"), while the catalog names ids
// ("claude-fable-5"), so a window is attributed to a model when the
// label appears as a word inside the id. A looser rule would refuse work
// on a model that is not actually spent, which is worse than the gap it
// closes: the product would sit idle with capacity in hand.

// ModelWindowFor returns the per-model window matching model, or nil.
// model is a catalog id; the window's own Model is the vendor's label.
func ModelWindowFor(u PlanUsage, model string) *PlanWindow {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return nil
	}
	for i := range u.Windows {
		w := &u.Windows[i]
		if w.Name != PlanWindowModelWeekly {
			continue
		}
		label := strings.ToLower(strings.TrimSpace(w.Model))
		if label == "" {
			continue
		}
		if modelIDMentions(id, label) {
			return w
		}
	}
	return nil
}

// modelIDMentions reports whether a catalog id names the labelled model.
// The label must appear delimited — "fable" matches "claude-fable-5" and
// "claude-fable-5[1m]" but not a longer word that merely contains it.
func modelIDMentions(id, label string) bool {
	for _, part := range strings.FieldsFunc(id, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '/' || r == ':' ||
			r == '[' || r == ']' || r == ' '
	}) {
		if part == label {
			return true
		}
	}
	return false
}

// ModelHasAvailableTokens reports whether model can accept a turn on this
// provider's plan. A model with no published window of its own is judged
// by the plan alone — absence of a per-model figure is not exhaustion.
func ModelHasAvailableTokens(u PlanUsage, model string, now time.Time, th *PlanThresholds) bool {
	if !HasAvailableTokens(u, now, th) {
		return false
	}
	w := ModelWindowFor(u, model)
	if w == nil || w.RemainingPercent == nil {
		return true
	}
	return *w.RemainingPercent > 0
}

// ExhaustedModels lists the vendor labels this snapshot reports as spent,
// for a caller that wants to say why rather than only that.
func ExhaustedModels(u PlanUsage) []string {
	var out []string
	for i := range u.Windows {
		w := &u.Windows[i]
		if w.Name != PlanWindowModelWeekly || w.RemainingPercent == nil {
			continue
		}
		if *w.RemainingPercent <= 0 && strings.TrimSpace(w.Model) != "" {
			out = append(out, w.Model)
		}
	}
	return out
}

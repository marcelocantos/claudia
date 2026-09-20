// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "strings"

// secondTurnWatcher reports, from the event stream alone, when the host has
// begun a turn after the first one. It is the live Goal journey's oracle for
// "the host continued without an external nudge" (🎯T39, 🎯T77).
//
// Counting non-terminal events after the first terminal cannot decide this,
// in either direction. Claude repeats one terminal message across its
// content blocks, so the first turn can end more than once and the events
// between those ends are still the FIRST turn — a false positive. And Claude
// can answer a continuation with a single terminal message carrying no
// non-terminal event at all — a false negative. The daemon's goal test was
// failed by both before it was keyed on turn id (10e7e44), so the rule here
// is turn identity: a turn after the first is one whose turn id differs from
// the first terminal's, or an event carrying the host's continuation prompt.
//
// Backends that leave TurnID empty fall back to the old activity rule.
// Which rule decided is reported, so a green run says how it was won.
type secondTurnWatcher struct {
	terminals int
	firstTurn string
	firstSeen bool

	// Second is the mark of the first event attributed to a later turn.
	Second turnMark
	// Rule names what decided: "turn-id", "continuation-prompt", or
	// "activity" for the fallback on a backend with no turn ids.
	Rule string
}

// turnMark identifies one observed point in the stream.
type turnMark struct {
	TurnID string
	Kind   string
}

// Observe folds one event in and reports whether this event is the one that
// first showed a later turn.
func (w *secondTurnWatcher) Observe(ev Event) bool {
	// The first terminal cannot be evidence of a turn after itself, so the
	// gate reads the state as it was BEFORE this event was folded in.
	afterFirst := w.firstSeen
	if ev.IsTerminalStop() {
		w.terminals++
		if w.terminals == 1 {
			w.firstSeen = true
			w.firstTurn = ev.TurnID
		}
	}
	if !afterFirst || w.Rule != "" {
		return false
	}
	// The continuation prompt is the host's own text going back in, so it
	// names a later turn whatever the backend does with ids.
	if strings.Contains(string(ev.Raw), goalContinuationMarker) {
		w.Second, w.Rule = turnMark{TurnID: ev.TurnID, Kind: ev.Type}, "continuation-prompt"
		return true
	}
	if w.firstTurn != "" {
		if ev.TurnID != "" && ev.TurnID != w.firstTurn {
			w.Second, w.Rule = turnMark{TurnID: ev.TurnID, Kind: ev.Type}, "turn-id"
			return true
		}
		return false
	}
	// No turn ids from this backend: the best that is left is activity
	// after the first terminal.
	if ev.Type == "assistant" || ev.ProgressType == "tool_use" {
		w.Second, w.Rule = turnMark{TurnID: ev.TurnID, Kind: ev.Type}, "activity"
		return true
	}
	return false
}

// FirstTurnID is the turn id of the first terminal, empty before one lands.
func (w *secondTurnWatcher) FirstTurnID() string { return w.firstTurn }

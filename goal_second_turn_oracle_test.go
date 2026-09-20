// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "testing"

// These are the two directions the old "any non-terminal event after the
// first terminal" rule got wrong, and the reason the live Goal journey is
// keyed on turn identity (🎯T77). Both are written from Claude's real
// stream shape: one logical assistant message arrives as several events,
// only the last of which carries a stop_reason.

func terminal(turnID string) Event {
	return Event{Type: "assistant", StopReason: "end_turn", TurnID: turnID}
}

func TestSecondTurnWatcherIgnoresTheFirstTurnEndingTwice(t *testing.T) {
	var w secondTurnWatcher
	// Claude repeats one terminal message across its content blocks: the
	// same turn ends, emits more of itself, and ends again. None of that
	// is a second turn.
	for _, ev := range []Event{
		{Type: "assistant", TurnID: "t1", Text: "ping"},
		terminal("t1"),
		{Type: "assistant", TurnID: "t1", Text: "ping"},
		terminal("t1"),
		{Type: "progress", ProgressType: "tool_use", TurnID: "t1"},
	} {
		if w.Observe(ev) {
			t.Fatalf("event %+v was read as a second turn; it is the first turn ending twice", ev)
		}
	}
	if w.Rule != "" {
		t.Fatalf("Rule=%q, want no decision yet", w.Rule)
	}
}

func TestSecondTurnWatcherSeesAContinuationThatIsOneTerminalMessage(t *testing.T) {
	var w secondTurnWatcher
	if w.Observe(terminal("t1")) {
		t.Fatal("the first terminal is not a second turn")
	}
	// The continuation answers with a single terminal message and no
	// non-terminal event at all. The activity rule never fires on this
	// stream; turn identity does.
	if !w.Observe(terminal("t2")) {
		t.Fatal("a terminal message on a new turn id is a second turn")
	}
	if w.Rule != "turn-id" {
		t.Fatalf("Rule=%q, want turn-id", w.Rule)
	}
	if w.Second.TurnID != "t2" {
		t.Fatalf("Second.TurnID=%q, want t2", w.Second.TurnID)
	}
}

func TestSecondTurnWatcherSeesTheContinuationPrompt(t *testing.T) {
	var w secondTurnWatcher
	w.Observe(terminal("t1"))
	ev := Event{Type: "user", TurnID: "t1", Raw: []byte(`{"text":"` + goalContinuation("do the thing") + `"}`)}
	if !w.Observe(ev) {
		t.Fatal("the host's own continuation prompt in the stream is a second turn")
	}
	if w.Rule != "continuation-prompt" {
		t.Fatalf("Rule=%q, want continuation-prompt", w.Rule)
	}
}

func TestSecondTurnWatcherWaitsForTheFirstTerminal(t *testing.T) {
	var w secondTurnWatcher
	// Events before any terminal belong to the first turn even when they
	// carry a different id; nothing has ended yet.
	if w.Observe(Event{Type: "assistant", TurnID: "t9", Text: "thinking"}) {
		t.Fatal("no turn has ended yet, so there is no second turn")
	}
}

func TestSecondTurnWatcherFallsBackWhenTheBackendHasNoTurnIDs(t *testing.T) {
	var w secondTurnWatcher
	w.Observe(terminal(""))
	if !w.Observe(Event{Type: "assistant", Text: "more"}) {
		t.Fatal("with no turn ids the activity rule is all that is left")
	}
	if w.Rule != "activity" {
		t.Fatalf("Rule=%q, want activity", w.Rule)
	}
}

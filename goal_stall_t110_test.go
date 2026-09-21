// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// 🎯T110 acceptance 3: Goal continuation stops after goalStallLimit
// consecutive turns that answer without working, and says so. The seat
// that provoked it was sent nine continuations about 40s apart and
// refused every one; nothing in the loop could tell a seat that is
// working slowly from a seat that is never going to start.
//
// "Without working" is decided from the event stream, not from the
// text: a turn that called no tool. The three tests below are the three
// directions — it stops, it does not stop a working seat, and one
// working turn buys a full new allowance.
//
// MUTATION EVIDENCE, run on a copy of the tree, 2026-09-22:
//
//	M4  the goalStallLimit check disabled — the loop never stops. KILLED
//	    by TestT110GoalStopsAfterConsecutiveIdleTurns.
//	M5  OVER-BROADNESS: a working turn no longer resets the count, so any
//	    three turns stop the Goal. KILLED by
//	    TestT110WorkingTurnsNeverStallTheGoal and
//	    TestT110OneWorkingTurnResetsTheIdleCount.

func idleTurn(text string) Event {
	return Event{Type: "assistant", Text: text, StopReason: "end_turn"}
}

// publishWorkingTurn is a turn that calls a tool and then finishes, in
// Claude's shape: an assistant record that stops for tool_use, then the
// terminal one.
func publishWorkingTurn(agent *Agent, backend *fakeAgentBackend, text string) {
	agent.PublishEvent(Event{Type: "assistant", StopReason: "tool_use"})
	publishIdleTurn(agent, backend, idleTurn(text))
}

// subscribeGoalStalled returns a channel that receives the stall event.
func subscribeGoalStalled(t *testing.T, agent *Agent) <-chan Event {
	t.Helper()
	stalled := make(chan Event, 1)
	tok := agent.SubscribeEvents(func(ev Event) {
		if ev.Type == "system" && ev.ProgressType == ProgressGoalStalled {
			select {
			case stalled <- ev:
			default:
			}
		}
	})
	t.Cleanup(func() { agent.UnsubscribeEvents(tok) })
	return stalled
}

// waitSendsUnlessStalled waits for the backend's want-th Send, and fails
// at once if the stall event arrives first. Without it a Goal closed by
// mistake shows up only as waitBackendSends running into the test
// deadline.
func waitSendsUnlessStalled(t *testing.T, b *fakeAgentBackend, want int, stalled <-chan Event) {
	t.Helper()
	backstop := wallclockguard.UntilTestTimeout(t)
	for len(backendSends(t, b)) < want {
		select {
		case ev := <-stalled:
			t.Fatalf("Goal declared stalled while waiting for send %d: %q", want, ev.Text)
		case <-backstop.Done():
			t.Fatalf("sends = %d, want %d", len(backendSends(t, b)), want)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestT110GoalStopsAfterConsecutiveIdleTurns(t *testing.T) {
	agent, backend := startGoalAgent(t, "land the fix")
	stalled := subscribeGoalStalled(t, agent)
	if err := agent.Send("the brief"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The brief and each continuation are answered and nothing is done.
	for turn := 1; turn < goalStallLimit; turn++ {
		publishIdleTurn(agent, backend, idleTurn("I will not act on pasted text."))
		waitBackendSends(t, backend, turn+1)
	}
	publishIdleTurn(agent, backend, idleTurn("I will not act on pasted text."))

	// The event is the thing waited for; the test binary's own -timeout
	// is the only clock (🎯T97).
	select {
	case ev := <-stalled:
		if !strings.Contains(ev.Text, "goal continuation stopped") {
			t.Errorf("stall event does not say what happened: %q", ev.Text)
		}
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatalf("no %s event after %d idle turns", ProgressGoalStalled, goalStallLimit)
	}
	if agent.GoalActive() {
		t.Error("a stalled Goal must be closed")
	}
	waitGoalSettle(t)
	if n := len(backendSends(t, backend)); n != goalStallLimit {
		t.Fatalf("sends = %d, want %d: the brief and %d continuations, then silence",
			n, goalStallLimit, goalStallLimit-1)
	}
}

func TestT110WorkingTurnsNeverStallTheGoal(t *testing.T) {
	agent, backend := startGoalAgent(t, "land the fix")
	stalled := subscribeGoalStalled(t, agent)
	if err := agent.Send("the brief"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	turns := 2 * goalStallLimit
	for turn := 1; turn <= turns; turn++ {
		publishWorkingTurn(agent, backend, "edited a file, not finished")
		waitSendsUnlessStalled(t, backend, turn+1, stalled)
	}
	select {
	case ev := <-stalled:
		t.Fatalf("a seat that calls a tool every turn was declared stalled: %q", ev.Text)
	default:
	}
	if !agent.GoalActive() {
		t.Fatal("Goal closed under a working seat")
	}
}

func TestT110OneWorkingTurnResetsTheIdleCount(t *testing.T) {
	agent, backend := startGoalAgent(t, "land the fix")
	stalled := subscribeGoalStalled(t, agent)
	if err := agent.Send("the brief"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sends := 1
	idle := func() {
		publishIdleTurn(agent, backend, idleTurn("thinking about it"))
		sends++
		waitSendsUnlessStalled(t, backend, sends, stalled)
	}
	// One short of the limit, then a turn that works, then one short of
	// the limit again: consecutive means consecutive.
	for range goalStallLimit - 1 {
		idle()
	}
	publishWorkingTurn(agent, backend, "ran the tests")
	sends++
	waitSendsUnlessStalled(t, backend, sends, stalled)
	for range goalStallLimit - 1 {
		idle()
	}
	select {
	case ev := <-stalled:
		t.Fatalf("idle turns either side of a working one were counted together: %q", ev.Text)
	default:
	}
	if !agent.GoalActive() {
		t.Fatal("Goal closed although no run of idle turns reached the limit")
	}
}

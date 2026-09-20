// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrTurnAbandoned ends a [Agent.WaitForResponse] whose turn went
// silent: nothing at all arrived from the agent — no event of any
// type, no terminal byte — for [Config.TurnSilenceBound], and no
// terminal stop_reason ever came.
//
// It is not "the turn took too long". A turn that is working is not
// silent, so the bound never fires on one; see turnSilenceBound.
var ErrTurnAbandoned = errors.New("turn went silent and never ended")

// ErrAgentGone ends a [Agent.WaitForResponse] whose agent died before
// the turn's terminal event arrived. Nothing can publish that event
// afterwards, so the wait is unsatisfiable and says so at once rather
// than sitting out any bound.
var ErrAgentGone = errors.New("agent died before the turn ended")

// turnSilenceBound is how long [Agent.WaitForResponse] will sit with
// NOTHING arriving from the agent before it declares the turn
// abandoned. Raise it per agent with [Config.TurnSilenceBound].
//
// It bounds SILENCE, not the turn. Every event of any type and every
// terminal byte rearms it, so a turn that is working — thinking,
// calling tools, repainting a pane — is never touched however long it
// runs. That distinction is what makes a bound safe here at all, and
// it is the same one cursorPromptSilenceBound argues for the first
// prompt of a Cursor session.
//
// The number is measured, and what was measured is SILENCE INSIDE
// HEALTHY TURNS, not turn length. Two live ACP sessions, each told to
// run `sleep 90` and then answer:
//
//	grok    turn 2m38.4s  141 events  longest gap with nothing in it 1m44.9s
//	cursor  turn 4m22.8s   24 events  longest gap with nothing in it 2m34.2s
//
// Both counted zero terminal chunks in the same runs — an ACP session
// has no pane to repaint — so on those backends that gap is total
// silence. Note it is not the tool's 90 seconds but 1.2x to 1.7x of
// it: the peer falls quiet before the call is announced and stays
// quiet after it returns.
//
// So the bound must clear the longest tool call a healthy turn may
// make, multiplied by that ratio. The longest routine one in this
// ecosystem is an agent running a full test gate: `go test` allows
// ten minutes by default and this repo's suite is sized against it —
// the run that opened 🎯T96 sat in the root package for the whole
// 600.5s before that timeout fired. Ten minutes at the worst observed
// ratio is ~17 minutes of silence; thirty clears it with room to
// spare, and [Config.TurnSilenceBound] is there for a consumer whose
// tools run longer still.
//
// The asymmetry says which way to err, as it did for the Cursor
// bound. A false conviction throws away a turn that was working and
// hands its consumer an error instead of an answer; a bound that
// waits longer than it strictly had to costs only lateness, on a turn
// that was already lost. And what it replaces is not a shorter wait —
// it is no answer at all, for the life of the process.
const turnSilenceBound = 30 * time.Minute

// turnLivenessPollInterval is how often a wait re-probes [Agent.Alive].
// Death is usually delivered, not polled — a closed ACP transport or a
// dropped broker socket wakes the wait immediately through markDead —
// but a killed tmux WINDOW is neither: control mode attaches to the
// session and keeps streaming after the window is gone (🎯T602, and
// internal/tmuxagent/control.go's own note that %exit handling is
// unimplemented). Only the window probe knows, and only when asked.
//
// Alive caches that probe for windowCheckTTL, so asking at the same
// cadence costs at most one tmux exec per seat per interval and
// usually none: the daemon's converge loop is already asking.
const turnLivenessPollInterval = windowCheckTTL

// turnWitness records what one [Agent.WaitForResponse] call has seen.
// A wait that ends without the turn's terminal event reports what it
// was waiting on — which turn, what last arrived, how long ago —
// instead of a bare timeout that names nothing.
type turnWitness struct {
	mu   sync.Mutex
	seen turnSeen
}

// turnSeen is the witness's record, copyable for reporting.
type turnSeen struct {
	events   int
	lastAt   time.Time
	lastType string
	turnID   string
	terminal bool
	chars    int
}

// note records one observed event.
func (w *turnWitness) note(ev Event, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen.events++
	w.seen.lastAt = at
	if ev.Type != "" {
		w.seen.lastType = ev.Type
	}
	if ev.TurnID != "" {
		w.seen.turnID = ev.TurnID
	}
	if ev.IsTerminalStop() {
		w.seen.terminal = true
	}
}

// lastEventAt reports when the last event arrived; zero means none.
func (w *turnWitness) lastEventAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen.lastAt
}

// sawTerminal reports whether a terminal stop_reason has arrived.
func (w *turnWitness) sawTerminal() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen.terminal
}

// snapshot copies the witness's record for reporting.
func (w *turnWitness) snapshot() turnSeen {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// turnWaitError explains a wait that ended without the turn's terminal
// event. cause is [ErrTurnAbandoned] or [ErrAgentGone]; the returned
// error wraps it, so callers can tell the two apart with errors.Is.
func (a *Agent) turnWaitError(cause error, w turnSeen, now, started, lastActivity time.Time, bound time.Duration) error {
	turn := w.turnID
	if turn == "" {
		turn = "(no turn id seen)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "claudia: WaitForResponse: session %s (%s) turn %s: %s",
		a.SessionID(), a.Provider(), turn, cause)
	fmt.Fprintf(&b, "; waited %s, saw %d event(s)", roundDur(now.Sub(started)), w.events)
	if w.events > 0 {
		fmt.Fprintf(&b, ", last %q %s ago", w.lastType, roundDur(now.Sub(w.lastAt)))
	}
	if w.chars > 0 {
		fmt.Fprintf(&b, ", %d char(s) of turn text", w.chars)
	}
	if w.terminal {
		b.WriteString(", terminal stop_reason seen but the turn never settled")
	} else {
		b.WriteString(", no terminal stop_reason")
	}
	if errors.Is(cause, ErrTurnAbandoned) {
		fmt.Fprintf(&b, "; nothing arrived for %s (silence bound %s — raise Config.TurnSilenceBound if this agent is legitimately silent for longer)",
			roundDur(now.Sub(lastActivity)), bound)
	}
	return fmt.Errorf("%s: %w", b.String(), cause)
}

// roundDur trims a duration to something a human reads in a log line.
func roundDur(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// now reads this agent's clock. A zero-value Agent (hermetic fixtures
// build one directly) reads the wall clock.
func (a *Agent) now() time.Time {
	if a.clk == nil {
		return time.Now()
	}
	return a.clk.Now()
}

// after is [Clock.After] for this agent, wall clock by default.
func (a *Agent) after(d time.Duration) <-chan time.Time {
	if a.clk == nil {
		return time.After(d)
	}
	return a.clk.After(d)
}

// silenceBound is the effective [Config.TurnSilenceBound].
func (a *Agent) silenceBound() time.Duration {
	a.mu.Lock()
	d := a.turnSilenceBound
	a.mu.Unlock()
	if d <= 0 {
		return turnSilenceBound
	}
	return d
}

// markDead records that this agent can no longer be reached and wakes
// anything waiting on it. Every assignment of alive = false goes
// through here or markDeadLocked: a death nobody can observe is how a
// wait with no other wake becomes permanent (🎯T96).
func (a *Agent) markDead() {
	a.mu.Lock()
	a.markDeadLocked()
	a.mu.Unlock()
}

// markDeadLocked is [Agent.markDead] for callers already holding a.mu.
func (a *Agent) markDeadLocked() {
	a.alive = false
	if a.dead != nil {
		select {
		case <-a.dead:
		default:
			close(a.dead)
		}
	}
}

// deadSignal returns a channel closed when this agent dies. It is
// created on demand, so an agent nobody waits on carries nothing; a
// nil return (agent already dead) is the caller's cue that there is
// nothing to wait for.
func (a *Agent) deadSignal() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dead == nil {
		a.dead = make(chan struct{})
		if !a.alive {
			close(a.dead)
		}
	}
	return a.dead
}

// lastTermActivity reports when this agent last emitted terminal
// bytes. Zero means never.
func (a *Agent) lastTermActivity() time.Time {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	return a.termActivityAt
}

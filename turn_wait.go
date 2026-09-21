// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"flag"
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

// ErrFramesDropped accompanies [ErrTurnAbandoned] when, during the wait,
// the broker connection carrying this seat skipped frames too large to
// relay (🎯T105). The silence bound still ends the wait — an unrelayable
// frame is one lost event, not a dead connection (🎯T73) — but the verdict
// ErrTurnAbandoned states, that nothing at all arrived, is then not known
// to be true: the turn's terminal event may be the frame that was lost.
// Callers tell a silent agent from a lost frame with errors.Is.
var ErrFramesDropped = errors.New("broker frames were dropped during the turn")

// ErrAgentGone ends a [Agent.WaitForResponse] whose agent died before
// the turn's terminal event arrived. Nothing can publish that event
// afterwards, so the wait is unsatisfiable and says so at once rather
// than sitting out any bound.
var ErrAgentGone = errors.New("agent died before the turn ended")

// ErrSilenceBoundCutShort accompanies [ErrTurnAbandoned] when the bound
// that fired was not the configured one but a shorter one, fitted to the
// deadline the wait was running under (🎯T103).
//
// The distinction is the whole reason this error exists. A conviction
// under the measured [turnSilenceBound] says the agent stopped talking
// for longer than a healthy turn ever does. A conviction under a cut
// bound says only that the wait ran out of PROCESS — `make live` runs
// with -timeout 30m, so a live test starting late in that binary can be
// handed a bound below the 2m34.2s of healthy silence measured off a real
// Cursor mint, and would convict a turn that was working.
//
// Cutting the bound is still right: the alternative is the process-wide
// panic, which reports nothing about anything. What is not right is
// letting the two convictions look identical. Callers tell them apart
// with errors.Is, and the message says which one it was either way.
var ErrSilenceBoundCutShort = errors.New("silence bound was cut short to fit the wait's own deadline")

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
// HEALTHY TURNS, not turn length. Four live ACP sessions, each told to
// run `sleep 90` and then answer — two mints per backend, so that no
// single observation decides this the way one did in 🎯T33:
//
//	grok    turn 2m38.4s  141 events  longest gap with nothing in it 1m44.9s
//	grok    turn 2m39.6s  203 events  longest gap with nothing in it 1m30.2s
//	cursor  turn 4m22.8s   24 events  longest gap with nothing in it 2m34.2s
//	cursor  turn 4m03.5s   27 events  longest gap with nothing in it 2m26.4s
//
// All four counted zero terminal chunks in the same runs — an ACP
// session has no pane to repaint — so on those backends that gap is
// total silence. Note it is not the tool's 90 seconds but 1.0x to
// 1.7x of it: the peer falls quiet before the call is announced and
// stays quiet after it returns. The two mints of a backend agree
// within 15%, which is what makes the ratio a property of the peer
// rather than of the afternoon.
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

// turnBound is the silence bound one wait ran under, together with what
// it was configured to be. The two differ only when a deadline cut it
// down, and a report that could not tell them apart would present a wait
// that ran out of process as an agent that went quiet.
type turnBound struct {
	// effective is the bound the wait actually armed.
	effective time.Duration
	// configured is [Agent.silenceBound] before any deadline cut it.
	configured time.Duration
	// deadline is what did the cutting; zero when nothing did.
	deadline time.Time
}

// cut reports whether a deadline shortened this bound.
func (b turnBound) cut() bool { return b.effective < b.configured }

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

// frameDrops is a count of broker frames skipped as unrelayable and the
// last refusal.
type frameDrops struct {
	n    int
	last error
}

// since is the drops that happened after start was taken.
func (d frameDrops) since(start frameDrops) frameDrops {
	if d.n <= start.n {
		return frameDrops{}
	}
	return frameDrops{n: d.n - start.n, last: d.last}
}

// turnWaitError explains a wait that ended without the turn's terminal
// event. cause is [ErrTurnAbandoned] or [ErrAgentGone]; the returned
// error wraps it, so callers can tell the two apart with errors.Is. drops
// are the broker frames lost during this wait.
func (a *Agent) turnWaitError(cause error, w turnSeen, now, started, lastActivity time.Time, bound turnBound, drops frameDrops) error {
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
		fmt.Fprintf(&b, "; nothing arrived for %s (silence bound %s",
			roundDur(now.Sub(lastActivity)), roundDur(bound.effective))
		if bound.cut() {
			fmt.Fprintf(&b, ", cut from the configured %s to fit this process's own deadline %s from now — the wait ran out of process, not out of patience, and a turn legitimately silent for longer is convicted here)",
				roundDur(bound.configured), roundDur(bound.deadline.Sub(now)))
		} else {
			b.WriteString(" — raise Config.TurnSilenceBound if this agent is legitimately silent for longer)")
		}
	} else {
		// A death, or a caller's context ending the wait. Neither is a
		// conviction, so neither names the bound — but how long the
		// agent had been quiet is the same question the reader has, and
		// answering it is why this error exists at all (🎯T103).
		fmt.Fprintf(&b, "; nothing arrived for %s", roundDur(now.Sub(lastActivity)))
	}
	abandonedWithDrops := errors.Is(cause, ErrTurnAbandoned) && drops.n > 0
	if abandonedWithDrops {
		fmt.Fprintf(&b, "; but %d broker frame(s) on this seat's connection were dropped as too large to relay during the wait (last: %v) — the turn's end may have been one of them, so this silence is not evidence the agent stopped",
			drops.n, drops.last)
	}
	// Every cause that applies is wrapped: the acceptance's ErrTurnAbandoned
	// still matches, and a caller that must know the bound was not the
	// measured one, or that frames were lost, can ask.
	format, args := "%s: %w", []any{b.String(), cause}
	if errors.Is(cause, ErrTurnAbandoned) && bound.cut() {
		format += ": %w"
		args = append(args, ErrSilenceBoundCutShort)
	}
	if abandonedWithDrops {
		format += ": %w"
		args = append(args, ErrFramesDropped)
	}
	return fmt.Errorf(format, args...)
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

// processStart is as close to this process's birth as this package can
// observe: its own package initialisation. Under `go test` the testing
// package arms its timeout later still, in m.Run, so a deadline measured
// from here lands slightly EARLY — the safe direction, since the cost of
// being early is a diagnosis a little sooner and the cost of being late
// is the panic this exists to beat.
var processStart = time.Now()

// processDeadline is when the process running a wait will be killed out
// from under it, if anything will.
//
// Under `go test` something will: the test binary's own -timeout panics
// the whole process, and takes every other test's result with it. That is
// 🎯T103. [turnSilenceBound] is thirty minutes because that is what
// healthy-turn silence measures; `go test` allows ten by default, so
// inside the suite — the one place an abandoned turn has actually bitten,
// twice — 🎯T96's named diagnosis was correct and unreachable. The wait
// knew the session, the turn, the last event and its age, and was never
// asked, because the process died first.
//
// Nothing else in this codebase sets such a deadline, so outside a test
// binary there is none and production bounds are untouched. The flag is
// testing's own, registered by testing.Init before any test runs and
// absent from any other build: asking for it is how this reads "am I
// inside a test binary, and how long does it have" without the product
// importing testing.
func processDeadline() (time.Time, bool) {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return time.Time{}, false
	}
	var d time.Duration
	if g, ok := f.Value.(flag.Getter); ok {
		d, _ = g.Get().(time.Duration)
	}
	if d == 0 {
		// -timeout 0 disables the panic entirely; so does a value this
		// cannot read, and both mean the same thing here — no deadline
		// to fit inside.
		parsed, err := time.ParseDuration(f.Value.String())
		if err != nil || parsed <= 0 {
			return time.Time{}, false
		}
		d = parsed
	}
	return processStart.Add(d), true
}

// waitDeadline is the instant this wait's process dies, when there is one
// and when this agent's timers are measured against the same clock.
//
// The second half is not a formality. A hermetic fixture runs a
// [ManualClock]: its bound is manual-clock time and the process deadline
// is wall-clock time, so subtracting one from the other would make the
// bound a function of how long the test BINARY had been running — a
// verdict decided by how fast the host is, which is the whole 🎯T33 /
// 🎯T92 / 🎯T93 family this fix must not rejoin. A wait on a manual clock
// is bounded by the test that drives it, and needs nothing from here.
func (a *Agent) waitDeadline() (time.Time, bool) {
	if !a.onWallClock() {
		return time.Time{}, false
	}
	return processDeadline()
}

// onWallClock reports whether this agent's timers run on real time.
func (a *Agent) onWallClock() bool {
	if a.clk == nil {
		return true
	}
	_, ok := a.clk.(SystemClock)
	return ok
}

// boundUnderDeadline shortens a silence bound to fit inside a deadline the
// wait cannot outlive, and never lengthens one.
//
// The reserve is half of what is left, and a proportion rather than a
// fixed margin on purpose: what the reserve has to cover is how much the
// process still has to do after this wait reports, which the wait cannot
// know. A proportion does not need to know it. Half is the one that makes
// the guarantee statable without knowing when the park happened —
// however late in a run a turn is abandoned, the wait hands back half of
// whatever budget remained, and a second park in the same run hands back
// half of that again.
//
// A deadline already past returns zero: the panic is imminent, so the
// only useful bound is now.
func boundUnderDeadline(bound time.Duration, now, deadline time.Time) time.Duration {
	left := deadline.Sub(now)
	if left <= 0 {
		return 0
	}
	if budget := left / 2; budget < bound {
		return budget
	}
	return bound
}

// waitBound is the silence bound one [Agent.WaitForResponse] call will
// actually run under: [Agent.silenceBound], shortened to fit inside the
// deadline the wait is living under. A bound that outlives its own
// deadline reports nothing at all, which is 🎯T103's whole defect.
func (a *Agent) waitBound(started time.Time) turnBound {
	configured := a.silenceBound()
	deadline, ok := a.waitDeadline()
	if !ok {
		return turnBound{effective: configured, configured: configured}
	}
	return turnBound{
		effective:  boundUnderDeadline(configured, started, deadline),
		configured: configured,
		deadline:   deadline,
	}
}

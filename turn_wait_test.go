// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 🎯T96. WaitForResponse used to have exactly two wakes: the caller's
// context and the turn's own terminal event. A consumer that passes a
// long-lived context — daemon code with context.Background(), a test with
// t.Context() — therefore had NO wake at all when a turn's terminal event
// never arrived, and the goroutine parked for the life of the process. The
// observed cost was a suite that died on `panic: test timed out after
// 10m0s` with the goroutine parked at the old select.
//
// Every test here drives a ManualClock. Nothing sleeps, so none of these
// verdicts can be decided by how fast the host is, which is the 🎯T33 /
// 🎯T92 / 🎯T93 family of mistakes this fix must not repeat.

type waitResult struct {
	text string
	err  error
}

// waitFixture is the smallest agent a wait can run against: no backend, no
// transport, no tmux window — just the event fan-out and a manual clock.
func waitFixture(clk *ManualClock, bound time.Duration, alive bool) *Agent {
	return &Agent{
		provider:         ProviderClaude,
		sessionID:        "t96-session",
		eventSubs:        make(map[int64]EventFunc),
		clk:              clk,
		turnSilenceBound: bound,
		alive:            alive,
	}
}

// startWait runs WaitForResponse against ctx-that-never-expires and returns
// once the wait's subscriber (and its liveness capture, which precedes it)
// is installed, so a test can publish or kill without racing the setup.
func startWait(t *testing.T, a *Agent) <-chan waitResult {
	t.Helper()
	res := make(chan waitResult, 1)
	go func() {
		// context.Background() is the point: the acceptance is a wait
		// that the caller's context cannot end. A deadline here would
		// test the deadline instead.
		text, err := a.WaitForResponse(context.Background())
		res <- waitResult{text, err}
	}()
	waitForEventSubscribers(t, a, 1)
	return res
}

// advanceUntilAnswered drives the clock forward until the wait answers. It
// spins on the answer rather than advancing a guessed number of times: the
// verdict is then the code's, not the scheduler's. A wait that never ends
// fails by `go test -timeout`, which dumps the goroutine parked inside
// WaitForResponse — the failure that names the defect.
func advanceUntilAnswered(clk *ManualClock, d time.Duration, res <-chan waitResult) waitResult {
	for {
		select {
		case r := <-res:
			return r
		default:
		}
		clk.Advance(d)
		runtime.Gosched()
	}
}

// fireAndRearm advances the clock far enough to fire the wait's silence
// timer and returns once the wait has armed a fresh one. Pending() is the
// synchronisation: the new timer cannot exist until the silence check has
// actually run and decided to keep waiting. An answer instead of a re-arm
// is the failure this guards.
func fireAndRearm(t *testing.T, clk *ManualClock, d time.Duration, res <-chan waitResult) {
	t.Helper()
	clk.Advance(d)
	for clk.Pending() == 0 {
		select {
		case r := <-res:
			t.Fatalf("the wait convicted a turn that was still producing: text=%q err=%v", r.text, r.err)
		default:
		}
		runtime.Gosched()
	}
}

// The acceptance: subscribe, dispatch a non-terminal update, never send a
// terminal — and the wait must END, naming what it was waiting for.
func TestWaitForResponseAbandonedTurnFailsNamingWhatItSaw(t *testing.T) {
	const bound = 90 * time.Second
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, bound, false)

	res := startWait(t, a)
	a.publishEvent(Event{
		Type:      "assistant",
		SessionID: "t96-session",
		TurnID:    "turn-42",
		Text:      "on it",
	})

	r := advanceUntilAnswered(clk, bound, res)
	if r.err == nil {
		t.Fatalf("a turn whose terminal event never arrived returned text %q and no error", r.text)
	}
	if !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned", r.err)
	}
	if errors.Is(r.err, context.DeadlineExceeded) || errors.Is(r.err, context.Canceled) {
		t.Fatalf("the caller's context ended the wait, which is the defect: %v", r.err)
	}
	// "Names the turn it was waiting for and what it last saw" — a bare
	// timeout that names nothing is what made the original failure take
	// two commits to attribute.
	for _, want := range []string{"t96-session", "turn-42", "assistant", "no terminal stop_reason"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error %q does not mention %q", r.err, want)
		}
	}
}

// A turn that is WORKING is not silent, and must never be convicted however
// long it runs. This is the half that a bare wall-clock turn timeout gets
// wrong: three full bounds of clock pass here while the agent keeps talking.
func TestWaitForResponseSilenceBoundRearmsOnEvents(t *testing.T) {
	const bound = time.Minute
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, bound, false)

	res := startWait(t, a)
	for i := 0; i < 3; i++ {
		a.publishEvent(Event{Type: "assistant", SessionID: "t96-session", Text: "still working"})
		// Just short of the bound, measured from the event: a bound on
		// silence restarts at the activity, not at the wait.
		fireAndRearm(t, clk, bound-time.Second, res)
	}

	// Now the turn goes quiet for real.
	r := advanceUntilAnswered(clk, bound, res)
	if !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned once the turn went silent", r.err)
	}
}

// The same, for terminal bytes. A Claude tool call can run for many minutes
// without a single JSONL line while the TUI repaints throughout; those bytes
// are the evidence the turn is alive. A bound that ignored them would
// convict exactly the longest healthy turns.
func TestWaitForResponseTerminalBytesAreTurnActivity(t *testing.T) {
	const bound = time.Minute
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, bound, false)

	res := startWait(t, a)
	for i := 0; i < 3; i++ {
		a.pushTermOutput([]byte("\x1b[2K· Thinking… (esc to interrupt)"))
		fireAndRearm(t, clk, bound-time.Second, res)
	}
	if r := advanceUntilAnswered(clk, bound, res); !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned once the terminal went quiet", r.err)
	}
}

// Death is the wake that needs no clock at all: a dead agent cannot publish
// the terminal event, so the wait is unsatisfiable the moment it dies.
//
// The clock is driven here anyway, past the bound, so that an agent whose
// death goes unobserved does not hang this test but fails it with the wrong
// verdict — ErrTurnAbandoned, "the turn went quiet", for an agent that is
// gone. The two cannot race to a wrong answer: the wait prefers death when
// both are true, because a dead agent's silence IS the death.
func TestWaitForResponseEndsWhenTheAgentDies(t *testing.T) {
	const bound = 30 * time.Second
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, bound, true)

	res := startWait(t, a)
	a.publishEvent(Event{Type: "assistant", SessionID: "t96-session", TurnID: "turn-7", Text: "half a th"})
	a.markDead()

	r := advanceUntilAnswered(clk, bound, res)
	if !errors.Is(r.err, ErrAgentGone) {
		t.Fatalf("err = %v, want errors.Is ErrAgentGone", r.err)
	}
	for _, want := range []string{"t96-session", "turn-7"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error %q does not mention %q", r.err, want)
		}
	}
}

// The wake itself, asserted structurally. The behavioural tests above can
// only observe a MISSING silence wake as a hang — which is the original
// defect's own signature, and takes a test timeout to see. This one sees it
// immediately: the wait arms its silence timer before it subscribes, so by
// the time a subscriber is visible the timer must be on the clock.
//
// Without it there is nothing to see. That is the whole trouble with this
// class of bug: an absent wake leaves no trace until a suite dies.
func TestWaitForResponseArmsASilenceWake(t *testing.T) {
	const bound = time.Minute
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, bound, false)

	res := startWait(t, a)
	if n := clk.Pending(); n == 0 {
		t.Fatal("the wait armed no silence wake: a turn whose terminal event never arrives would have nothing to end it")
	}
	// Leave nothing parked behind the test.
	advanceUntilAnswered(clk, bound, res)
}

// The death signal is the mechanism the wait above consumes: an alive flag
// that flips with nobody able to observe it is how a wait with no other wake
// becomes permanent.
func TestMarkDeadClosesTheDeathSignal(t *testing.T) {
	a := waitFixture(NewManualClock(time.Now()), time.Hour, true)
	dead := a.deadSignal()
	a.markDead()
	select {
	case <-dead:
	default:
		t.Fatal("markDead left the death signal open: a wait on this agent has nothing to wake it")
	}
	if a.Alive() {
		t.Fatal("markDead left the agent reporting alive")
	}
}

// A death that lands after the turn actually ended is not a lost turn. The
// settle timer exists to catch trailing content blocks, and a dead agent
// sends none — so the answer is delivered, not discarded.
func TestWaitForResponseDeathAfterTerminalStillReturnsTheTurn(t *testing.T) {
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, time.Hour, true)

	res := startWait(t, a)
	a.publishEvent(parseEvent(`{"type":"assistant","message":{"model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"pong"}]}}`))
	a.markDead()

	r := <-res
	if r.err != nil {
		t.Fatalf("a completed turn was reported as a failure: %v", r.err)
	}
	if !strings.Contains(r.text, "pong") {
		t.Fatalf("text = %q, want the turn's reply", r.text)
	}
}

// An agent that was already dead before the wait began keeps the old
// behaviour: the death is its birth state, not an event, so the silence
// bound is what ends the wait. Every bare hermetic fixture in this package
// is in exactly that state, and none of them may start failing with
// ErrAgentGone.
func TestWaitForResponseOnDeadAgentUsesTheSilenceBound(t *testing.T) {
	const bound = 30 * time.Second
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, bound, false)

	res := startWait(t, a)
	r := advanceUntilAnswered(clk, bound, res)
	if !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned", r.err)
	}
}

// Config.TurnSilenceBound is the consumer's lever for an agent whose healthy
// turns really are silent for longer; zero keeps the package default.
func TestTurnSilenceBoundIsConfigurable(t *testing.T) {
	a := waitFixture(NewManualClock(time.Now()), 0, false)
	if got := a.silenceBound(); got != turnSilenceBound {
		t.Fatalf("default silenceBound = %v, want %v", got, turnSilenceBound)
	}
	a.turnSilenceBound = 3 * time.Hour
	if got := a.silenceBound(); got != 3*time.Hour {
		t.Fatalf("configured silenceBound = %v, want 3h", got)
	}
}

// The death wake is only as good as the coverage of markDead: a site that
// sets alive = false by hand kills the agent without telling anyone, and the
// waits on that agent go back to having nothing to wake them. There were six
// such sites when 🎯T96 was written, across five files, and nothing said so.
func TestEveryDeathGoesThroughMarkDead(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var offenders []string
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// markDeadLocked is the one place that may write the flag.
		if name == "turn_wait.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "alive = false") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", name, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files; the guard is not looking at anything")
	}
	if len(offenders) > 0 {
		t.Fatalf("death recorded without waking anything waiting on it — use markDead / markDeadLocked:\n\t%s",
			strings.Join(offenders, "\n\t"))
	}
}

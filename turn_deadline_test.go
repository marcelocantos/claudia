// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🎯T103. 🎯T96 gave WaitForResponse a wake for a turn whose terminal event
// never arrives, and a named error — ErrTurnAbandoned, carrying the session,
// the turn, what last arrived and how long ago. The bound behind that wake is
// thirty minutes, measured against healthy-turn silence and load-bearing (see
// turnSilenceBound). `go test` allows ten. So in the one place the defect had
// actually bitten, twice, the diagnosis was correct and unreachable: the
// process died on `panic: test timed out` first, taking every other test's
// result with it.
//
// The repair does not touch the production number. It makes the bound answer
// to the deadline the wait is running under — the test binary's timeout when
// there is one, the caller's context when it has a deadline — because a bound
// that outlives its own deadline reports nothing at all.

// The arithmetic, in isolation. The reserve is half of what is left, and the
// bound is never lengthened by it.
func TestBoundUnderDeadlineReservesHalfAndNeverLengthens(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		what  string
		bound time.Duration
		left  time.Duration
		want  time.Duration
	}{
		{"a bound far longer than the deadline is cut to half of it", 30 * time.Minute, 10 * time.Minute, 5 * time.Minute},
		{"a bound that already fits is left alone", 90 * time.Second, 10 * time.Minute, 90 * time.Second},
		{"a bound equal to the reserve is left alone", 5 * time.Minute, 10 * time.Minute, 5 * time.Minute},
		{"a deadline already past leaves no time to wait at all", 30 * time.Minute, -time.Second, 0},
		{"a deadline exactly now is the same", 30 * time.Minute, 0, 0},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if got := boundUnderDeadline(tc.bound, now, now.Add(tc.left)); got != tc.want {
				t.Fatalf("boundUnderDeadline(%v, now, now+%v) = %v, want %v", tc.bound, tc.left, got, tc.want)
			}
		})
	}
}

// The deadline this binary is running under is the -timeout it was given,
// measured from as close to the binary's birth as the package can see.
// Measuring from package init rather than from m.Run puts the deadline
// slightly EARLY, which is the safe direction: early costs a diagnosis
// sooner, late costs the panic this exists to beat.
func TestProcessDeadlineIsTheTestBinaryTimeout(t *testing.T) {
	deadline, ok := processDeadline()
	f := flag.Lookup("test.timeout")
	if f == nil {
		t.Fatal("test.timeout is not registered inside a test binary; the deadline this fix reads does not exist")
	}
	timeout, err := time.ParseDuration(f.Value.String())
	if err != nil {
		t.Fatalf("parse test.timeout %q: %v", f.Value.String(), err)
	}
	if timeout <= 0 {
		if ok {
			t.Fatalf("-timeout %v disables the panic, so there is no deadline to fit inside, but processDeadline reported %v", timeout, deadline)
		}
		t.Skipf("suite running with -timeout %v: no process deadline to fit inside", timeout)
	}
	if !ok {
		t.Fatalf("running under -timeout %v and processDeadline reported none: an abandoned turn would still outlive the binary", timeout)
	}
	if want := processStart.Add(timeout); !deadline.Equal(want) {
		t.Fatalf("processDeadline = %v, want processStart+%v = %v", deadline, timeout, want)
	}
}

// A wait on a real clock inside a test binary is bounded by that binary, not
// by the thirty minutes it was configured with. This is the acceptance's
// "answers to the deadline it is actually running under", asserted on the
// number itself rather than on how long anything took.
func TestWaitBoundFitsInsideTheTestBinaryTimeout(t *testing.T) {
	deadline, ok := processDeadline()
	if !ok {
		t.Skip("suite running with -timeout 0: no process deadline to fit inside")
	}
	a := &Agent{
		provider:         ProviderClaude,
		sessionID:        "t103-bound",
		eventSubs:        make(map[int64]EventFunc),
		clk:              SystemClock{},
		turnSilenceBound: turnSilenceBound,
	}
	now := time.Now()
	got := a.waitBound(now)
	if got.effective >= turnSilenceBound {
		t.Fatalf("waitBound = %v, the full production bound, inside a binary that has %v left: the diagnosis cannot arrive",
			got.effective, roundDur(deadline.Sub(now)))
	}
	if want := boundUnderDeadline(turnSilenceBound, now, deadline); got.effective != want {
		t.Fatalf("waitBound = %v, want %v", got.effective, want)
	}
	if !got.cut() {
		t.Fatalf("waitBound did not record that %v was cut from the configured %v", got.effective, got.configured)
	}
}

// The timeline guard, and it is not a formality: the process deadline is
// wall-clock time and a fixture's bound is manual-clock time, so a wait that
// mixed them would have its verdict decided by how long the test BINARY had
// been running. Every silence test in this package would then pass or fail on
// how far into the suite it ran — the 🎯T33 / 🎯T92 / 🎯T93 mistake, rebuilt.
func TestAWaitOnAManualClockIgnoresTheProcessDeadline(t *testing.T) {
	if _, ok := processDeadline(); !ok {
		t.Skip("suite running with -timeout 0: nothing for the guard to keep out")
	}
	a := waitFixture(NewManualClock(time.Now()), turnSilenceBound, false)
	if d, ok := a.waitDeadline(); ok {
		t.Fatalf("a manual-clock wait took the wall-clock deadline %v: its bound would now depend on how long the binary had been running", d)
	}
	got := a.waitBound(a.now())
	if got.effective != turnSilenceBound {
		t.Fatalf("waitBound = %v on a manual clock, want the configured %v untouched", got.effective, turnSilenceBound)
	}
	if got.cut() {
		t.Fatal("waitBound reported a cut on a manual clock, where no wall-clock deadline applies")
	}
}

// The caller's context is the other deadline a wait runs under, and a bare
// context.DeadlineExceeded names nothing — not the session, not the turn, not
// what last arrived. The cause is still wrapped, so a caller testing for it
// keeps working.
//
// The context here is already expired, and the cancelled case below needs no
// clock either: neither verdict can be decided by how fast the host is.
func TestWaitForResponseNamesTheTurnWhenTheCallersDeadlineEnds(t *testing.T) {
	a := waitFixture(NewManualClock(time.Now()), time.Hour, false)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := a.WaitForResponse(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want errors.Is context.DeadlineExceeded", err)
	}
	for _, want := range []string{"t96-session", "no terminal stop_reason", "nothing arrived for"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The same for a cancelled wait, with a turn in flight: the report names the
// turn and the last event, which is what a caller has to go on.
func TestWaitForResponseNamesTheTurnWhenTheCallerCancels(t *testing.T) {
	a := waitFixture(NewManualClock(time.Now()), time.Hour, false)
	ctx, cancel := context.WithCancel(context.Background())

	res := make(chan waitResult, 1)
	go func() {
		text, err := a.WaitForResponse(ctx)
		res <- waitResult{text, err}
	}()
	waitForEventSubscribers(t, a, 1)
	a.publishEvent(Event{Type: "assistant", SessionID: "t96-session", TurnID: "turn-103", Text: "on it"})
	cancel()

	r := <-res
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is context.Canceled", r.err)
	}
	for _, want := range []string{"t96-session", "turn-103", "assistant"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error %q does not mention %q", r.err, want)
		}
	}
}

// t103ChildEnv marks the re-executed test binary below. It carries no "LIVE"
// word on purpose: the live-gate census reads environment variables out of
// the source to decide what is a live test (internal/livegate), and these two
// spend nothing and reach no backend.
const t103ChildEnv = "CLAUDIA_T103_CHILD"

// t103ChildTimeout is the deadline the child binary is given. The wait inside
// it is configured with the full production thirty minutes, so before this
// fix the child could only die on `panic: test timed out`; with it, the bound
// becomes half of what the child has left and the wait answers at roughly
// half this, leaving the other half for the rest of the child's run.
const t103ChildTimeout = 8 * time.Second

// The acceptance, end to end, on the real path: a parked turn inside a test
// binary produces a NAMED failure and the rest of the binary still reports.
//
// It re-executes this same test binary rather than compiling a fixture, so
// what is measured is a real `go test` deadline against the real wait — the
// two things whose relationship is the defect. The child's own verdict is the
// assertion; the parent reads its output.
//
// What this one cannot escape is real time: it asserts that the wait's answer
// beats the child's timeout, and a machine stalled for the ~4s of slack would
// report the panic instead. That is the irreducible part of demonstrating
// "the process did not die", and it is why every OTHER test here is decided
// by arithmetic instead.
func TestT103AbandonedTurnIsNamedInsideThePackageTimeout(t *testing.T) {
	if os.Getenv(t103ChildEnv) != "" {
		t.Skip("this is the child binary; its two cases are the assertion")
	}
	bin := os.Args[0]
	if !filepath.IsAbs(bin) {
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatalf("locate this test binary (%q): %v", bin, err)
		}
		bin = abs
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("this test binary is not where os.Args[0] says (%q): %v", bin, err)
	}

	cmd := exec.Command(bin,
		"-test.run=^TestT103Child",
		"-test.timeout="+t103ChildTimeout.String(),
		"-test.v")
	cmd.Env = append(os.Environ(), t103ChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "test timed out") {
		t.Fatalf("the child died on the process-wide timeout, which is the defect: the wait never said what it was waiting for\n%s", got)
	}
	if err != nil {
		t.Fatalf("child run failed (%v):\n%s", err, got)
	}
	for _, want := range []string{
		"--- PASS: TestT103ChildParkedWaitIsNamedNotPanicked",
		"--- PASS: TestT103ChildStillReportsAfterTheParkedWait",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("child output has no %q:\n%s", want, got)
		}
	}
}

// The park itself, run in the child. A fake-backed agent on the real clock,
// configured with the shipped thirty-minute bound, whose turn opens and then
// never ends. Nothing here waits on the host: whether this passes turns on
// whether the bound was fitted to the binary's deadline at all.
func TestT103ChildParkedWaitIsNamedNotPanicked(t *testing.T) {
	if os.Getenv(t103ChildEnv) == "" {
		t.Skip("child of TestT103AbandonedTurnIsNamedInsideThePackageTimeout")
	}
	a := &Agent{
		provider:         ProviderClaude,
		sessionID:        "t103-session",
		eventSubs:        make(map[int64]EventFunc),
		clk:              SystemClock{},
		turnSilenceBound: turnSilenceBound,
	}

	res := make(chan waitResult, 1)
	go func() {
		// context.Background() is the shape the defect took at
		// cursor_stuck_prompt_test.go:87 — a wait with no deadline of its
		// own, inside a binary that has one.
		text, err := a.WaitForResponse(context.Background())
		res <- waitResult{text, err}
	}()
	waitForEventSubscribers(t, a, 1)
	a.publishEvent(Event{Type: "assistant", SessionID: "t103-session", TurnID: "turn-103", Text: "on it"})

	r := <-res
	if !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned", r.err)
	}
	for _, want := range []string{"t103-session", "turn-103", "assistant", "no terminal stop_reason"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error %q does not mention %q", r.err, want)
		}
	}
	t.Logf("the wait's own diagnosis: %v", r.err)
}

// "…and the rest of its package still reports." This test has nothing to do
// but exist after the parked one: its PASS line in the child's output is the
// half of the acceptance that the panic used to destroy.
func TestT103ChildStillReportsAfterTheParkedWait(t *testing.T) {
	if os.Getenv(t103ChildEnv) == "" {
		t.Skip("child of TestT103AbandonedTurnIsNamedInsideThePackageTimeout")
	}
}

// A conviction under a CUT bound is not the same claim as a conviction
// under the measured one, and must not read like it.
//
// This is not hypothetical arithmetic. `make live` runs with -timeout 30m
// and every live backend shares that one binary, so a live test starting
// late in the run is handed a bound of a couple of minutes — below the
// 2m34.2s of healthy silence measured off a real Cursor mint
// (turn_wait.go's turnSilenceBound). Cutting is still better than the
// process-wide panic, which reports nothing at all; presenting the result
// as "the agent went quiet for longer than a healthy turn ever does" is
// not, because that sends the reader after the wrong defect.
//
// Both causes are wrapped, so 🎯T103's acceptance (errors.Is
// ErrTurnAbandoned) holds while a caller that needs to know can ask.
func TestAConvictionUnderACutBoundSaysSo(t *testing.T) {
	a := waitFixture(NewManualClock(time.Now()), turnSilenceBound, false)
	now := a.now()
	// The shape of a live test starting 27 minutes into `make live`.
	bound := turnBound{
		effective:  90 * time.Second,
		configured: turnSilenceBound,
		deadline:   now.Add(3 * time.Minute),
	}
	if !bound.cut() {
		t.Fatal("a bound of 90s against a configured 30m does not report itself as cut")
	}

	err := a.turnWaitError(ErrTurnAbandoned, turnSeen{events: 2, lastAt: now.Add(-90 * time.Second), lastType: "assistant", turnID: "turn-live"},
		now, now.Add(-2*time.Minute), now.Add(-90*time.Second), bound)

	if !errors.Is(err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned — 🎯T103's acceptance names that error", err)
	}
	if !errors.Is(err, ErrSilenceBoundCutShort) {
		t.Fatalf("err = %v, want errors.Is ErrSilenceBoundCutShort: a caller cannot tell a short budget from a silent agent", err)
	}
	for _, want := range []string{"cut from the configured 30m", "ran out of process, not out of patience"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

// The other half: a conviction under the bound the consumer configured
// carries no such qualifier, and must not claim one. A message that cried
// "cut short" on every abandoned turn would be as useless as one that
// never did.
func TestAConvictionUnderTheConfiguredBoundClaimsNoCut(t *testing.T) {
	a := waitFixture(NewManualClock(time.Now()), turnSilenceBound, false)
	now := a.now()
	bound := turnBound{effective: turnSilenceBound, configured: turnSilenceBound}
	if bound.cut() {
		t.Fatal("an uncut bound reports itself as cut")
	}

	err := a.turnWaitError(ErrTurnAbandoned, turnSeen{events: 1, lastAt: now.Add(-turnSilenceBound), lastType: "assistant", turnID: "turn-7"},
		now, now.Add(-turnSilenceBound), now.Add(-turnSilenceBound), bound)

	if errors.Is(err, ErrSilenceBoundCutShort) {
		t.Fatalf("err = %v claims its bound was cut short when it ran the configured %v", err, turnSilenceBound)
	}
	if !strings.Contains(err.Error(), "raise Config.TurnSilenceBound") {
		t.Errorf("error %q drops the lever a consumer with legitimately slower turns needs", err)
	}
}

// The live gate's own arithmetic, pinned. `make live` passes -timeout 30m
// (Makefile), and what that leaves a wait late in the run is the number
// that decides whether a healthy Cursor turn survives. This test does not
// forbid the cut — it fails if anyone believes the cut bound still clears
// measured healthy silence, because it does not, which is exactly why the
// error above has to say so.
func TestTheLiveGateBudgetCanFallBelowMeasuredHealthySilence(t *testing.T) {
	// Measured off a real Cursor mint; see turnSilenceBound's table.
	const measuredCursorSilence = 2*time.Minute + 34*time.Second
	const liveGateTimeout = 30 * time.Minute

	start := time.Now()
	deadline := start.Add(liveGateTimeout)
	late := start.Add(27 * time.Minute)

	got := boundUnderDeadline(turnSilenceBound, late, deadline)
	if got >= measuredCursorSilence {
		t.Fatalf("a wait starting 27m into `make live` gets %v, which clears the measured %v — "+
			"if that is now true, ErrSilenceBoundCutShort's warning is overstated and should be revisited",
			got, measuredCursorSilence)
	}
	t.Logf("27m into `make live` a wait is bounded at %v, below the measured %v: a healthy Cursor turn is convicted there, and the error says so",
		got, measuredCursorSilence)
}

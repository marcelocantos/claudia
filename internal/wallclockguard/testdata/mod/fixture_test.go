package fixture

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestUnmarked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second) // VIOLATION unmarked
	defer cancel()
	_ = ctx
}

// 🎯T97 exemption: the whole function's clocks are answered for here.
func TestFuncDoc(t *testing.T) {
	<-time.After(time.Millisecond)
}

// 🎯T97 exemption: a doc comment answers for its function even when the
// signature runs over several lines, so the body is not the next line.
func helperWithLongSignature(
	t *testing.T,
	d time.Duration,
) {
	<-time.After(d)
}

func TestLineAbove(t *testing.T) {
	// 🎯T97 exemption: the comment above the statement.
	<-time.After(time.Millisecond)
	<-time.After(time.Millisecond) // VIOLATION: the marker above answers for one statement only
}

func TestSameLine(t *testing.T) {
	<-time.After(time.Millisecond) // 🎯T97 exemption: a trailing comment on the clock's line.
}

func TestMultiLineStatement(t *testing.T) {
	// 🎯T97 exemption: the comment above a statement answers for a clock
	// inside it, however many lines down.
	select {
	case <-make(chan struct{}):
	case <-time.After(time.Millisecond):
	}
}

// 🎯T97 exemption: one reason for a constant, wherever it is used.
const failsafe = time.Second

func TestConstDecl(t *testing.T) {
	<-time.After(failsafe)
	tm := time.NewTimer(2 * failsafe)
	tm.Stop()
}

func TestNoReason(t *testing.T) {
	// 🎯T97 exemption
	<-time.After(time.Millisecond) // VIOLATION: a reasonless marker exempts nothing
}

func TestProse(t *testing.T) {
	// Prose that names a 🎯T97 exemption mid-line is not one.
	<-time.After(time.Millisecond) // VIOLATION
}

func TestStale(t *testing.T) {
	// 🎯T97 exemption: nothing below is a clock, so this is reported.
	time.Sleep(time.Millisecond)
}

func TestLiveBackend(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("live")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // live: out of scope
	defer cancel()
	_ = ctx
}

func helper() {
	_ = time.NewTicker(time.Second) // VIOLATION: helpers are scanned too
}

func ExampleNeverRuns() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // no output comment: never run
	defer cancel()
	_ = ctx
}

func ExampleRuns() {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now()) // VIOLATION: this one runs
	defer cancel()
	_ = ctx
	// Output:
}

// 🎯T106: deadlines and elapsed bounds assembled by hand from time.Now().

func TestPollLoop(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) { // VIOLATION: a poll loop on a hand-built deadline
	}
}

func TestDeadlineCheck(t *testing.T) {
	start := time.Now()
	deadline := start.Add(time.Second)
	if time.Now().After(deadline) { // VIOLATION: taint flows through start.Add
		t.Fatal("late")
	}
	if deadline.Before(time.Now()) { // VIOLATION: now as the argument
		t.Fatal("late")
	}
}

func TestElapsedBound(t *testing.T) {
	start := time.Now()
	if elapsed := time.Since(start); elapsed > time.Second { // VIOLATION: elapsed upper bound
		t.Fatal("slow")
	}
	if float64(time.Since(start).Milliseconds()) > 1000 { // VIOLATION: through a method and a conversion
		t.Fatal("slow")
	}
}

func measured() time.Duration {
	start := time.Now()
	return time.Since(start)
}

func measuredTwice() time.Duration { return measured() }

func TestHelperReturnsElapsed(t *testing.T) {
	if measuredTwice() > time.Second { // VIOLATION: a helper of a helper returns the clock
		t.Fatal("slow")
	}
}

func TestNowUsesThatDecideNothing(t *testing.T) {
	start := time.Now()
	t.Logf("took %v", time.Since(start))
	var samples []time.Duration
	samples = append(samples, time.Since(start))
	stamp := struct{ at time.Time }{}
	stamp.at = time.Now().Add(-time.Minute)
	n, limit := 3, 5
	if n > limit {
		t.Fatal("not a clock")
	}
	_ = samples
}

func TestMarkedLowerBound(t *testing.T) {
	start := time.Now()
	// 🎯T97 exemption: a lower bound; a slow host can only pass it.
	if time.Since(start) < time.Millisecond {
		t.Fatal("too fast")
	}
}

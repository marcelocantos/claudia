// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
	"time"
)

// 🎯T31 — a wall clock may bound cleanup; it may not decide a verdict.
//
// A readiness wait ("the test cannot proceed until X has happened") that
// carries its own deadline measures the machine, not the product. Under fleet
// load — a dozen concurrent agents plus a second -race suite — the deadline is
// what fails, and the test reports RED for work it never exercised. That is a
// self-owned gate input: the number deciding the pass is set by whoever else
// happens to be busy. Widening the constant does not fix it, it only moves the
// load at which it fires (the prohibition 🎯T17 carried, and the reason
// waitHermeticTaskReady's 5s deadline drew a spurious RED on 2026-08-15).
//
// The rule this file implements:
//
//   - A readiness wait is driven by a signal the fake or the product emits, or
//     it is unbounded. A slow machine makes it slower, never RED.
//   - The single backstop for a genuine hang is `go test -timeout`, which fires
//     once per suite and dumps every goroutine — it names the real culprit,
//     where a per-helper deadline only names the helper.
//   - A context timeout that bounds process cleanup and is never expected to
//     fire is fine; one whose expiry can produce a failing assertion is not.
//     Prefer context.WithCancel + defer cancel(), which cleans up just as well
//     and cannot expire early.
//   - A sleep that is itself the thing under test (a debounce or settle window)
//     is load-bearing and stays, sized off the product's own constant.

// waitForCond blocks until cond returns true, with no deadline. what names the
// condition so a `go test -timeout` goroutine dump is self-explaining.
//
// The poll interval only bounds how promptly a satisfied condition is noticed;
// it never decides the verdict.
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		time.Sleep(time.Millisecond)
	}
	t.Logf("waitForCond: %s satisfied", what)
}

// timedGap sleeps for d and reports how long the sleep actually took.
//
// Some product behaviour is only observable across a real gap — a settle
// window that must be reset by an event arriving before it expires. The gap
// is a *precondition* of such a test ("the second event landed inside the
// window"), never an assertion. Under load the scheduler overshoots it, and a
// test that treats the overshoot as a result reports RED for a product that
// behaved correctly on inputs it was never given.
//
// Callers pair this with a retry: if the observed gap exceeded the window, the
// scenario never ran, so run it again. A slow machine retries; only a product
// that mishandles a gap it did receive fails (🎯T31).
func timedGap(d time.Duration) time.Duration {
	start := time.Now()
	time.Sleep(d)
	return time.Since(start)
}

// hermeticTaskWatch drains a Task's event stream and reports readiness from the
// stream itself rather than from a clock.
//
// Task publishes each event through recordTaskEvent before handing it to the
// caller's channel (task.go), so a TaskEventInit observed here means ClaudeID
// is already set *and* the slow fake has printed its first line — which is what
// Cancel/Stop actually depend on, since the fake installs its SIGINT handler
// before printing. Watching the stream is therefore a strictly stronger signal
// than polling ClaudeID, and it is causal rather than temporal.
type hermeticTaskWatch struct {
	ready   chan struct{} // closed when TaskEventInit is seen
	drained chan struct{} // closed when the event channel closes
}

// watchHermeticTask starts draining events immediately. Both of its outcomes
// are decided by the stream: init arrives, or the stream ends without it.
func watchHermeticTask(events <-chan TaskEvent) *hermeticTaskWatch {
	w := &hermeticTaskWatch{
		ready:   make(chan struct{}),
		drained: make(chan struct{}),
	}
	go func() {
		defer close(w.drained)
		sawInit := false
		for ev := range events {
			if !sawInit && ev.Type == TaskEventInit {
				sawInit = true
				close(w.ready)
			}
		}
	}()
	return w
}

// waitReady blocks until the fake emits init. There is no deadline: a loaded
// machine makes this slower, never RED. A stream that ends without init is a
// real defect — the fake never started — and fails immediately rather than
// after a timeout, so the failure is faster as well as truthful.
func (w *hermeticTaskWatch) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-w.ready:
		return
	case <-w.drained:
	}
	// close(ready) happens-before close(drained) in the watch goroutine, so a
	// stream that emitted init on its final events is still a pass.
	select {
	case <-w.ready:
	default:
		t.Fatal("hermetic task stream closed without TaskEventInit — the fake never started")
	}
}

// waitDrained blocks until the event channel closes, with no deadline. Used
// where the product's contract is "this channel closes once the process is
// signalled": a Cancel that never lands hangs until `go test -timeout` prints
// the goroutine that is stuck, which is the diagnosis a 5s deadline withheld.
func (w *hermeticTaskWatch) waitDrained() {
	<-w.drained
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"testing"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// TestHermeticTestsHaveNoWallClockDeadline keeps 🎯T33 fixed.
//
// TestHermeticTaskRunSuccess used to bound its run with
// context.WithTimeout(..., 10*time.Second). That context reaches the child
// through Run's runCtx (task.go:149) and exec.CommandContext (task.go:175),
// so when the deadline expired os/exec killed the fake CLI. The stdout
// scanner (task.go:221) then saw EOF having read nothing, and the ExitError
// that Wait produced raced runCtx.Done() in the send at task.go:273 — so the
// channel could close with no events at all. The failure was
// `incomplete events init=false result=false got=[]codex.Event(nil)`: not a
// truncated stream, an empty one, and the constant rather than the product
// had decided the verdict. On a loaded machine 10s is not enough to fork
// /bin/sh and read a fixture, so a green suite was not citable evidence.
//
// The fix is to wait on the event the test needs — drain the channel until
// the process closes it — and let `go test -timeout` be the only clock. This
// guard stops a deadline being reintroduced into a hermetic test, where a
// slow machine could again outvote the assertion.
//
// Live tests are exempt: they drive a real backend over the network, where a
// deadline bounds a genuinely unbounded wait rather than a local fixture
// read. 🎯T97 moved the scan into internal/wallclockguard so this package and
// the root one share it, which also made "live" mean what livegate's census
// says rather than a filename, and let a defensible clock carry an in-source
// exemption instead of being banned outright.
//
// This guard sees clocks a test writes for itself. It cannot see one a test
// merely arms: 🎯T93 was a product timeout in package claudia that
// codex_session_test.go assigned 200ms, with nothing deadline-shaped in the
// test source at all. That half is guarded by
// TestHermeticTestsDeclareTheProductBoundsTheyShorten in the root package.
func TestHermeticTestsHaveNoWallClockDeadline(t *testing.T) {
	rep, err := wallclockguard.Scan("..", ".")
	if err != nil {
		t.Fatal(err)
	}
	// A guard that scanned nothing would pass forever.
	if rep.Scanned == 0 {
		t.Fatal("no hermetic test files scanned — guard is not looking at anything")
	}
	for _, c := range rep.Violations {
		t.Errorf("%s: hermetic test uses %s — %s (🎯T33). "+
			"Wait on the event the test needs; let `go test -timeout` be the clock.",
			c.Pos(), c.Call, wallclockguard.Banned[c.Call])
	}
	for _, m := range rep.Markers {
		t.Errorf("%s: %s (🎯T97)", m.Pos(), m.Why)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// TestHermeticTestsHaveNoWallClockDeadline is 🎯T33's guard widened to the
// whole module (🎯T97): the root package, where most of claudia's hermetic
// tests live, and every other package the go command compiles. codex/ keeps
// its own copy because 🎯T33's mutation evidence runs there.
//
// 🎯T33 wrote it for codex/ alone and deliberately stopped there: the root
// package then carried 69 of these clocks, and not all of them were defects,
// so a ban would have been the wrong shape. The scanner now tells the two
// apart by making each surviving clock answer for itself — a clock that a
// slow host cannot turn into a wrong verdict carries a 🎯T97 exemption naming
// why, and every other one fails here. The rules live in
// internal/wallclockguard.
//
// This half sees clocks a test writes. Clocks a test arms by shortening a
// product bound are TestHermeticTestsDeclareTheProductBoundsTheyShorten's.
func TestHermeticTestsHaveNoWallClockDeadline(t *testing.T) {
	rep, err := wallclockguard.ScanModule(".")
	if err != nil {
		t.Fatal(err)
	}
	// A guard that scanned nothing would pass forever.
	if rep.Scanned == 0 || rep.ScannedDirs == 0 {
		t.Fatal("no hermetic test files scanned — guard is not looking at anything")
	}
	for _, c := range rep.Violations {
		t.Errorf("%s: hermetic test uses %s — %s (🎯T97). Wait on the event the test "+
			"needs and let `go test -timeout` be the clock, or, if a slow host cannot "+
			"turn this clock into a wrong verdict, say why in a %q comment.",
			c.Pos(), c.Call, wallclockguard.Banned[c.Call], wallclockguard.Marker)
	}
	for _, m := range rep.Markers {
		t.Errorf("%s: %s (🎯T97)", m.Pos(), m.Why)
	}
	t.Logf("%d hermetic test files scanned in %d packages, %d clocks exempted in source",
		rep.Scanned, rep.ScannedDirs, len(rep.Exempted))
}

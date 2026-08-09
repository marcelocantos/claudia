// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"strconv"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The pool's two timing decisions — has a warm window expired, and what
// deadline does keep_alive_for stamp — used to read time.Now() inline, which
// made them testable only by sleeping through a real TTL. They now read the
// injected Clock (🎯T2.8), so both are decidable here in microseconds. These are
// the seeded oracles 🎯T2.5's model-weighted reap thresholds will extend.

// poolEpoch is a fixed instant so every assertion is on an exact value.
var poolEpoch = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

// TestPoolKeepAliveDeadlineIsClockDriven proves a keep_alive_for release stamps
// its deadline from the injected clock, and that swapping the clock moves the
// deadline — the property that makes the reap path testable without waiting.
func TestPoolKeepAliveDeadlineIsClockDriven(t *testing.T) {
	clk := broker.NewManualClock(poolEpoch)
	if got, want := poolKeepAliveDeadline(clk.Now(), 300), poolEpoch.Add(5*time.Minute).Unix(); got != want {
		t.Fatalf("deadline at epoch = %d, want %d", got, want)
	}

	clk.Advance(90 * time.Second)
	if got, want := poolKeepAliveDeadline(clk.Now(), 300), poolEpoch.Add(90*time.Second+5*time.Minute).Unix(); got != want {
		t.Fatalf("deadline after +90s = %d, want %d", got, want)
	}
}

// TestPoolWindowExpiredFiresExactlyAtTheDeadline pins the boundary: a window is
// live right up to its deadline and expired from that second on. Off-by-one
// here either kills warm windows early (losing the pool's whole point) or leaks
// them past their TTL.
func TestPoolWindowExpiredFiresExactlyAtTheDeadline(t *testing.T) {
	clk := broker.NewManualClock(poolEpoch)
	deadline := strconv.FormatInt(poolKeepAliveDeadline(clk.Now(), 300), 10)

	clk.Advance(299 * time.Second)
	if poolWindowExpired(clk.Now(), deadline, true) {
		t.Error("window expired at +299s; deadline is +300s")
	}

	clk.Advance(time.Second)
	if !poolWindowExpired(clk.Now(), deadline, true) {
		t.Error("window still live at +300s; it should have expired exactly then")
	}

	clk.Advance(time.Hour)
	if !poolWindowExpired(clk.Now(), deadline, true) {
		t.Error("window came back to life an hour past its deadline")
	}
}

// TestPoolWindowExpiredTreatsBadInputAsNoDeadline checks the failure direction
// that actually costs something: a missing or unparseable deadline must never
// read as "expired", because the sweep that follows kills the window.
func TestPoolWindowExpiredTreatsBadInputAsNoDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		val string
		has bool
	}{
		"option absent":  {"", false},
		"empty value":    {"", true},
		"not a number":   {"soon", true},
		"zero":           {"0", true},
		"negative":       {"-1", true},
		"trailing junk":  {"1767225600x", true},
		"whitespace pad": {"  0  ", true},
	} {
		t.Run(name, func(t *testing.T) {
			if poolWindowExpired(poolEpoch.Add(100*time.Hour), tc.val, tc.has) {
				t.Errorf("deadline %q (present=%v) read as expired; a parse slip must not license a kill", tc.val, tc.has)
			}
		})
	}
}

// TestPoolWindowExpiredAcceptsPaddedDeadline confirms the guard above is not so
// blunt that it rejects a legitimate value tmux hands back with whitespace.
func TestPoolWindowExpiredAcceptsPaddedDeadline(t *testing.T) {
	clk := broker.NewManualClock(poolEpoch)
	deadline := "  " + strconv.FormatInt(poolKeepAliveDeadline(clk.Now(), 60), 10) + "\n"
	clk.Advance(61 * time.Second)
	if !poolWindowExpired(clk.Now(), deadline, true) {
		t.Errorf("padded deadline %q was not honoured", deadline)
	}
}

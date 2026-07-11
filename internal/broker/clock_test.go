// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"testing"
	"time"
)

// TestManualClockAdvanceFiresTimers is the seam's own oracle: it proves a
// model-weighted idle-reap TTL can be exercised deterministically — the timer
// stays silent before its deadline and fires exactly when Advance crosses it,
// with no wall-clock sleeping.
func TestManualClockAdvanceFiresTimers(t *testing.T) {
	start := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	c := NewManualClock(start)

	reap := c.After(5 * time.Minute) // opus-style idle TTL

	c.Advance(4 * time.Minute)
	select {
	case <-reap:
		t.Fatal("timer fired early at +4m (deadline is +5m)")
	default:
	}

	c.Advance(1 * time.Minute)
	select {
	case got := <-reap:
		if want := start.Add(5 * time.Minute); !got.Equal(want) {
			t.Fatalf("timer fired at %v, want %v", got, want)
		}
	default:
		t.Fatal("timer did not fire after crossing +5m deadline")
	}

	if got, want := c.Now(), start.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("Now()=%v, want %v", got, want)
	}
}

func TestManualClockZeroDurationFiresImmediately(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	select {
	case <-c.After(0):
	default:
		t.Fatal("After(0) should fire immediately")
	}
}

// TestManualClockOrdersMultipleTimers checks that a single Advance fires only
// the timers whose deadlines it crosses, in the same call — the property the
// reaper relies on when several agents share one tick.
func TestManualClockOrdersMultipleTimers(t *testing.T) {
	c := NewManualClock(time.Unix(0, 0))
	opus := c.After(5 * time.Minute)
	sonnet := c.After(15 * time.Minute)
	haiku := c.After(60 * time.Minute)

	c.Advance(15 * time.Minute)

	assertFired := func(name string, ch <-chan time.Time, want bool) {
		t.Helper()
		select {
		case <-ch:
			if !want {
				t.Fatalf("%s reaped too early", name)
			}
		default:
			if want {
				t.Fatalf("%s should have reaped by +15m", name)
			}
		}
	}
	assertFired("opus", opus, true)
	assertFired("sonnet", sonnet, true)
	assertFired("haiku", haiku, false)
}

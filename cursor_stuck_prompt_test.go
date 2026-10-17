// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
	"testing/synctest"
	"time"
)

// cursorHermeticPeerBound is the silence bound to give a fake peer that
// has to ANSWER inside it, rather than one whose silence is the point.
//
// The shipped bound carries a measured margin: 120s against a worst
// observed first inbound message of 18.3s, ~6.5x (cursor_acp.go). A
// hermetic test that keeps the mechanism and throws the ratio away is
// not a scaled model of the product — it is a tighter experiment that
// fails for a reason the product would not, and its RED is the host's
// scheduler rather than the code's behaviour.
//
// 🎯T92 is the specimen. This file shortened the bound to 400ms and the
// failing run observed 402ms: a margin of 1.0x, on a host at load 122.
// The package went RED, `make gate` lost its citation, and the defect
// the run had actually found took a second reading to see.
//
// 402ms is the worst turnaround ever observed from the python fake, and
// it was observed under deliberate saturation. This is 6.5x of that,
// rounded up — the product's own ratio, applied to the fake's own
// measurement.
const cursorHermeticPeerBound = 3 * time.Second

// shortenCursorSilenceBound makes the 🎯T83 bound testable. The shipped
// value is measured against real mints (~6.5x the worst observed first
// response) and no hermetic test should sit through it.
//
// A test whose peer must stay SILENT for the bound may pass any small
// value: no scheduler can make silence arrive late. A test whose peer
// must SPEAK inside the bound passes cursorHermeticPeerBound, or states
// its own margin over what it asked the peer to do.
func shortenCursorSilenceBound(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorPromptSilenceBound
	cursorPromptSilenceBound = d
	t.Cleanup(func() { cursorPromptSilenceBound = prev })
}

// A dead transport must end the wait, not sit out the full bound.
func TestCursorStuckPromptWaitEndsWhenTransportDies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &cursorACPClient{peerWoke: make(chan struct{}, 1)}
		done := make(chan struct{})
		go func() {
			c.awaitPeerActivity(0, time.Hour)
			close(done)
		}()
		// Release and join even a mutant that ignores transport closure,
		// including when an assertion below terminates the test goroutine.
		defer func() {
			c.notePeerActivity()
			<-done
		}()

		began := time.Now()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("awaitPeerActivity returned before the transport closed")
		default:
		}

		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.wakePromptWaiters()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("awaitPeerActivity did not return when the transport closed")
		}
		if elapsed := time.Since(began); elapsed != 0 {
			t.Fatalf("transport closure advanced virtual time by %v", elapsed)
		}
	})
}

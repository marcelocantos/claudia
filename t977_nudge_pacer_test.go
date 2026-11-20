// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// jevons 🎯T977: restart nudges leave at least NudgeSpacing apart, however
// many seats are ready together, so a resumed fleet does not reach its
// provider as one burst. The clock records the wait the pacer asks for: a
// wall-clock timestamp taken after pace returns measured the scheduler too,
// and under load two seats released 40ms apart were stamped 36µs apart.
func TestT977NudgesLeaveSpacedApart(t *testing.T) {
	const spacing = 40 * time.Millisecond
	clock := &recordingClock{now: time.Now()}
	pace := nudgePacer(clock, spacing)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pace(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// The first seat leaves at once; each of the others waits one more slot.
	got := clock.waits()
	slices.Sort(got)
	if want := []time.Duration{spacing, 2 * spacing, 3 * spacing}; !slices.Equal(got, want) {
		t.Fatalf("pacer waits %v, want %v", got, want)
	}

	// Zero spacing never waits; a cancelled resume stops waiting.
	if err := nudgePacer(SystemClock{}, 0)(context.Background()); err != nil {
		t.Fatal(err)
	}
	slow := nudgePacer(SystemClock{}, time.Hour)
	_ = slow(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := slow(ctx); err == nil {
		t.Fatal("a cancelled resume kept waiting for its nudge slot")
	}
}

// recordingClock stands still and fires every After at once, recording the
// duration asked for.
type recordingClock struct {
	now   time.Time
	mu    sync.Mutex
	asked []time.Duration
}

func (c *recordingClock) Now() time.Time { return c.now }

func (c *recordingClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.asked = append(c.asked, d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- c.now.Add(d)
	return ch
}

func (c *recordingClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

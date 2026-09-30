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
// provider as one burst. The assertion is a lower bound on the gaps: a slow
// host can only widen them.
func TestT977NudgesLeaveSpacedApart(t *testing.T) {
	const spacing = 40 * time.Millisecond
	pace := nudgePacer(spacing)
	var mu sync.Mutex
	var at []time.Time
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pace(context.Background()); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortFunc(at, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < spacing-5*time.Millisecond {
			t.Fatalf("nudges %d and %d left %v apart, want at least %v", i-1, i, gap, spacing)
		}
	}

	// Zero spacing never waits; a cancelled resume stops waiting.
	if err := nudgePacer(0)(context.Background()); err != nil {
		t.Fatal(err)
	}
	slow := nudgePacer(time.Hour)
	_ = slow(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := slow(ctx); err == nil {
		t.Fatal("a cancelled resume kept waiting for its nudge slot")
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// TestRunModelIntelRefresherCadence (🎯T75.7): once at start, once per
// interval under a manual clock, errors reported, and it returns on cancel.
func TestRunModelIntelRefresherCadence(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	var runs int
	var errs []error
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunModelIntelRefresher(ctx, &ModelIntelRefresherArgs{
			Interval: time.Hour,
			Clock:    clock,
			Refresh: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				runs++
				return errors.New("no key")
			},
			OnError: func(err error) {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			},
		})
	}()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return runs
	}
	waitFor(t, "refresh at start", func() bool { return count() == 1 })
	// The runner registers its timer only after Refresh returns. Advancing
	// before that leaves the timer to be created past the advance, where it
	// never fires: on a loaded host this test hung for 504 s into the package
	// timeout (owner gate 708bfe8c, 2026-09-22). Wait for the timer itself.
	waitFor(t, "the interval timer is registered", func() bool { return clock.Pending() == 1 })
	clock.Advance(59 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if got := count(); got != 1 {
		t.Fatalf("refreshed %d times before the interval elapsed", got)
	}
	clock.Advance(time.Minute)
	waitFor(t, "refresh after one interval", func() bool { return count() == 2 })
	cancel()
	select {
	case <-done:
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatal("refresher did not return on cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 2 {
		t.Fatalf("errors reported = %d, want 2", len(errs))
	}
}

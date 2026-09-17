// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

//claudia:policy

import (
	"context"
	"time"
)

// ModelIntelRefresherArgs configures [RunModelIntelRefresher]. The zero
// value refreshes the default store once a day on the wall clock.
type ModelIntelRefresherArgs struct {
	// Dir is the store. Empty uses [DefaultModelIntelDir].
	Dir string
	// Interval between refreshes. Zero uses DefaultModelIntelInterval.
	Interval time.Duration
	// Clock times the loop. Nil uses the wall clock.
	Clock Clock
	// Refresh replaces [RefreshModelIntel] (tests).
	Refresh func(ctx context.Context) error
	// OnError receives each failed refresh. A missing API key is a failed
	// refresh, so an application without one will see it every interval.
	OnError func(error)
}

// RunModelIntelRefresher refreshes the model-intel store once at start and
// then every interval until ctx is done (🎯T75.7). [Resolve] reads that
// store for purpose-quality floors; an application that embeds claudia
// runs this to keep it current, as the daemon does.
func RunModelIntelRefresher(ctx context.Context, args *ModelIntelRefresherArgs) {
	if args == nil {
		args = &ModelIntelRefresherArgs{}
	}
	clock := args.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	interval := args.Interval
	if interval <= 0 {
		interval = DefaultModelIntelInterval
	}
	refresh := args.Refresh
	if refresh == nil {
		refresh = func(ctx context.Context) error {
			_, err := RefreshModelIntel(ctx, &ModelIntelArgs{Dir: args.Dir, Now: clock.Now()})
			return err
		}
	}
	for {
		if err := refresh(ctx); err != nil && args.OnError != nil && ctx.Err() == nil {
			args.OnError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-clock.After(interval):
		}
	}
}

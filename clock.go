// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// Clock is the time source claudia's timed loops read: the Registry's
// liveness watch, the plan-usage monitor, the model-intel refresher, and the
// warm pool. Passing a [ManualClock] makes their cadence deterministic in
// tests. Policy code reads time only through a Clock; the claudia:policy
// guard fails the build otherwise (🎯T2.8).
type Clock = broker.Clock

// SystemClock is the wall clock.
type SystemClock = broker.SystemClock

// ManualClock is a [Clock] that moves only when told to.
type ManualClock = broker.ManualClock

// NewManualClock returns a [ManualClock] reading t until advanced.
func NewManualClock(t time.Time) *ManualClock { return broker.NewManualClock(t) }

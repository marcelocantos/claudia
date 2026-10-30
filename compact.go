// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrCompactNoEligibleFold is returned when the conversation is over the bound
// but nothing can be folded to satisfy it (🎯T244).
var ErrCompactNoEligibleFold = errors.New("compact: no eligible fold")

// ErrCompactCanceled is returned when cancellation settled with no commit.
var ErrCompactCanceled = errors.New("compact: canceled")

// ErrCompactUncertain is returned when cancel settlement timed out or history may have changed.
var ErrCompactUncertain = errors.New("compact: outcome uncertain")

// SeatCompact is the observed outcome of a one-shot Registry.Compact (🎯T244).
type SeatCompact struct {
	TokensBefore int
	TokensAfter  int
	Committed    bool
}

// CompactTimeout bounds a bound compaction end to end (native compaction lane).
const CompactTimeout = 15 * time.Minute

// CompactSettleTimeout bounds sidecar acknowledgment after caller cancel.
const CompactSettleTimeout = 30 * time.Second

// Compact folds the named seat's conversation toward boundTokens in one
// synchronous call between turns (🎯T244 / Jevons T1374). Only sidecar seats
// support it. The seat must be running and attached; a turn in flight refuses
// with [ErrTurnInFlight] before any mutation.
func (r *Registry) Compact(ctx context.Context, name string, boundTokens int) (SeatCompact, error) {
	if boundTokens <= 0 {
		return SeatCompact{}, fmt.Errorf("Compact %s: boundTokens must be positive", name)
	}
	def := r.Def(name)
	if def == nil {
		return SeatCompact{}, fmt.Errorf("agent %q not registered", name)
	}
	if !useOMP(Config{Provider: def.Provider}) {
		return SeatCompact{}, fmt.Errorf("Compact %s: only sidecar seats support bound compaction", name)
	}
	r.mu.Lock()
	proc := r.procs[name]
	r.mu.Unlock()
	if proc == nil || !proc.Alive() {
		return SeatCompact{}, fmt.Errorf("Compact %s: seat is not running", name)
	}
	if proc.PromptInFlight() {
		return SeatCompact{}, fmt.Errorf("Compact %s: %w", name, ErrTurnInFlight)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, CompactTimeout)
	defer cancel()
	out, err := proc.Compact(ctx, boundTokens)
	if err != nil {
		if errors.Is(err, ErrCompactNoEligibleFold) {
			return out, fmt.Errorf("Compact %s: %w", name, ErrCompactNoEligibleFold)
		}
		return out, err
	}
	return out, nil
}

// Compact runs a one-shot bound compaction on this seat (🎯T244).
func (a *Agent) Compact(ctx context.Context, boundTokens int) (SeatCompact, error) {
	if boundTokens <= 0 {
		return SeatCompact{}, fmt.Errorf("Compact: boundTokens must be positive")
	}
	<-a.ready
	if a.readyErr != nil {
		return SeatCompact{}, fmt.Errorf("agent not ready: %w", a.readyErr)
	}
	if !useOMP(Config{Provider: a.Provider(), OMP: a.startCfg.OMP}) {
		return SeatCompact{}, fmt.Errorf("Compact: only sidecar seats support bound compaction")
	}
	if a.PromptInFlight() {
		return SeatCompact{}, fmt.Errorf("Compact: %w", ErrTurnInFlight)
	}
	if a.ops.compact == nil {
		return SeatCompact{}, unsupportedCapability(a.Provider(), CapabilityCompact, "provider did not supply a compact operation")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, CompactTimeout)
	defer cancel()
	return a.ops.compact(a, ctx, boundTokens)
}

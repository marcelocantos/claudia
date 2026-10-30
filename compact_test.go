// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestRegistryCompactRefusesNonPositiveBound(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Compact(context.Background(), "missing", 0); err == nil {
		t.Fatal("want error for bound 0")
	}
}

func TestAgentCompactMapsTurnInFlight(t *testing.T) {
	var flying atomic.Bool
	flying.Store(true)
	a := &Agent{
		ready:    make(chan struct{}),
		provider: ProviderCursor,
		startCfg: Config{OMP: true},
		ops: agentOps{
			promptInFlight: func(*Agent) bool { return flying.Load() },
			compact: func(*Agent, context.Context, int) (SeatCompact, error) {
				t.Fatal("compact must not run")
				return SeatCompact{}, nil
			},
		},
	}
	close(a.ready)
	_, err := a.Compact(context.Background(), 180_000)
	if !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("Compact = %v, want ErrTurnInFlight", err)
	}
}

func TestAgentCompactReturnsObservedSizes(t *testing.T) {
	a := &Agent{
		ready:    make(chan struct{}),
		provider: ProviderCursor,
		startCfg: Config{Provider: ProviderCursor, OMP: true},
		ops: agentOps{
			promptInFlight: func(*Agent) bool { return false },
			compact: func(_ *Agent, _ context.Context, bound int) (SeatCompact, error) {
				if bound != 180_000 {
					t.Fatalf("bound = %d", bound)
				}
				return SeatCompact{TokensBefore: 200_000, TokensAfter: 120_000, Committed: true}, nil
			},
		},
	}
	close(a.ready)
	out, err := a.Compact(context.Background(), 180_000)
	if err != nil {
		t.Fatal(err)
	}
	if out.TokensBefore != 200_000 || out.TokensAfter != 120_000 || !out.Committed {
		t.Fatalf("out = %+v", out)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

//claudia:policy

import (
	"context"
	"sync"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The daemon's plan-usage evaluator (🎯T2.9). One process on the host
// fetches vendor usage; every consumer reads the snapshot over the socket.
// Time is read only through broker.Clock (the //claudia:policy marker enrols
// this file in the T2.8 guard), so the refresh cadence and the
// 429-invalidation path run under a ManualClock in tests.

// brokerUsageService holds the snapshot and runs the refresh loop.
type brokerUsageService struct {
	clock broker.Clock
	ttl   time.Duration
	fetch func(context.Context) ([]PlanUsage, error)
	// onUpdate runs after every completed fetch (tail event emission).
	onUpdate func(fetched []PlanUsage, err error)

	mu        sync.Mutex
	snapshot  []PlanUsage
	fetchedAt time.Time
	lastErr   string
	inflight  bool
	waiters   []chan struct{}
	// kick wakes the loop early: a forced refresh, or a 429 seen on a seat.
	kick chan struct{}
}

func newBrokerUsageService(clock broker.Clock, ttl time.Duration, fetch func(context.Context) ([]PlanUsage, error)) *brokerUsageService {
	if ttl <= 0 {
		ttl = DefaultPlanCacheTTL
	}
	if fetch == nil {
		fetch = defaultDaemonUsageFetch
	}
	return &brokerUsageService{clock: clock, ttl: ttl, fetch: fetch, kick: make(chan struct{}, 1)}
}

// defaultDaemonUsageFetch is QueryAllPlanUsage for every supported
// provider. Grok and Cursor unofficial surfaces are always fetched;
// a break is unavailable-with-reason, never gated off.
func defaultDaemonUsageFetch(ctx context.Context) ([]PlanUsage, error) {
	return QueryAllPlanUsage(ctx, &AllPlanUsageArgs{})
}

// run refreshes on start, then every ttl, or sooner when kicked.
func (u *brokerUsageService) run(ctx context.Context) {
	for {
		u.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-u.kick:
		case <-u.clock.After(u.ttl):
		}
	}
}

// refresh performs one fetch unless one is already in flight, in which
// case it waits for that one.
func (u *brokerUsageService) refresh(ctx context.Context) {
	u.mu.Lock()
	if u.inflight {
		ch := make(chan struct{})
		u.waiters = append(u.waiters, ch)
		u.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return
	}
	u.inflight = true
	u.mu.Unlock()

	fetched, err := u.fetch(ctx)

	u.mu.Lock()
	u.inflight = false
	if err == nil {
		u.snapshot = fetched
		u.fetchedAt = u.clock.Now()
		u.lastErr = ""
	} else {
		u.lastErr = err.Error()
	}
	waiters := u.waiters
	u.waiters = nil
	onUpdate := u.onUpdate
	u.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	if onUpdate != nil {
		onUpdate(fetched, err)
	}
}

// invalidate asks for an early refresh: a seat reported a rate limit or a
// quota stop, so the snapshot is wrong until re-read.
func (u *brokerUsageService) invalidate() {
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// read returns the snapshot, refreshing first when asked or when it is
// older than ttl. A snapshot that has never been fetched and cannot be
// fetched now returns the fetch error text with a zero fetchedAt.
func (u *brokerUsageService) read(ctx context.Context, force bool) ([]PlanUsage, time.Time, string) {
	u.mu.Lock()
	stale := u.fetchedAt.IsZero() || u.clock.Now().Sub(u.fetchedAt) > u.ttl
	u.mu.Unlock()
	if force || stale {
		u.refresh(ctx)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]PlanUsage, len(u.snapshot))
	copy(out, u.snapshot)
	return out, u.fetchedAt, u.lastErr
}

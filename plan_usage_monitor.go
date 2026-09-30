// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

//claudia:policy

import (
	"context"
	"sync"
	"time"
)

// PlanUsageMonitor keeps a plan-usage snapshot current (🎯T75.4, 🎯T2.9):
// it fetches on start and every TTL, lets one fetch run at a time, and
// fetches early when invalidated. The daemon runs one for the host; an
// application that embeds claudia can run its own. Time is read only
// through its Clock (the claudia:policy marker enrols this file in the
// 🎯T2.8 guard), so the refresh cadence and the 429 path run under a
// ManualClock in tests.
type PlanUsageMonitor struct {
	clock    Clock
	ttl      time.Duration
	fetch    func(context.Context) ([]PlanUsage, error)
	force    func(context.Context) ([]PlanUsage, error)
	onUpdate func(fetched []PlanUsage, err error)

	mu        sync.Mutex
	snapshot  []PlanUsage
	fetchedAt time.Time
	lastErr   string
	inflight  bool
	// inflightForced says the fetch in flight is a forced one. A forced
	// Read that finds an unforced fetch in flight cannot take its answer:
	// that fetch ran under the full request floor and may have carried
	// held readings forward rather than asking anyone.
	inflightForced bool
	waiters        []chan struct{}
	// kick wakes the loop early: a forced refresh, or a 429 seen on a seat.
	kick chan struct{}
}

// PlanUsageMonitorArgs configures [NewPlanUsageMonitor]. The zero value
// queries every provider every [DefaultPlanCacheTTL] on the wall clock.
type PlanUsageMonitorArgs struct {
	// TTL is how long a snapshot is current. Zero uses DefaultPlanCacheTTL.
	TTL time.Duration
	// Fetch replaces [QueryAllPlanUsage] for every provider (tests).
	Fetch func(context.Context) ([]PlanUsage, error)
	// ForceFetch is the producer for a forced [PlanUsageMonitor.Read].
	// Nil uses Fetch when Fetch is set, or [QueryAllPlanUsage] with
	// Forced so the request floor shortens to PlanThrottleForcedInterval.
	ForceFetch func(context.Context) ([]PlanUsage, error)
	// Clock times the loop and stamps snapshots. Nil uses the wall clock.
	Clock Clock
	// OnUpdate runs after every completed fetch, with its result.
	OnUpdate func(fetched []PlanUsage, err error)
}

// PlanUsageSnapshot is one [PlanUsageMonitor.Read].
type PlanUsageSnapshot struct {
	Backends []PlanUsage
	// FetchedAt is zero when no fetch has succeeded yet.
	FetchedAt time.Time
	// Err is the text of the most recent failed fetch, cleared by a
	// successful one.
	Err string
}

// NewPlanUsageMonitor returns a monitor. It fetches nothing until [Run] or
// [PlanUsageMonitor.Read].
func NewPlanUsageMonitor(args *PlanUsageMonitorArgs) *PlanUsageMonitor {
	if args == nil {
		args = &PlanUsageMonitorArgs{}
	}
	m := &PlanUsageMonitor{
		clock: args.Clock, ttl: args.TTL, fetch: args.Fetch, force: args.ForceFetch,
		onUpdate: args.OnUpdate,
		kick:     make(chan struct{}, 1),
	}
	if m.clock == nil {
		m.clock = SystemClock{}
	}
	if m.ttl <= 0 {
		m.ttl = DefaultPlanCacheTTL
	}
	if m.force == nil && m.fetch != nil {
		m.force = m.fetch
	}
	if m.fetch == nil {
		// Grok and Cursor unofficial surfaces are always fetched; a break
		// is unavailable-with-reason, never gated off.
		m.fetch = func(ctx context.Context) ([]PlanUsage, error) {
			return QueryAllPlanUsage(ctx, DefaultPlanFetchArgs())
		}
	}
	if m.force == nil {
		// A forced read is a caller saying "now" (a cockpit reload,
		// `claudia usage --refresh`). Without Forced the fetch obeys the
		// five-minute floor, which the loop's own TTL fetch has always
		// just reset, so every forced read re-served held readings.
		m.force = func(ctx context.Context) ([]PlanUsage, error) {
			args := DefaultPlanFetchArgs()
			args.Forced = true
			return QueryAllPlanUsage(ctx, args)
		}
	}
	return m
}

// Run refreshes on start, then every TTL or sooner when invalidated, until
// ctx is done. While it runs, the monitor is this process's plan-usage
// evaluator: [LoadPlanUsage] with default arguments reads it when no daemon
// answers, and a rate-limit or quota stop on any in-process agent
// invalidates it.
func (m *PlanUsageMonitor) Run(ctx context.Context) {
	unregister := registerPlanUsageMonitor(m)
	defer unregister()
	for {
		m.refresh(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
		case <-m.clock.After(m.ttl):
		}
	}
}

// refresh performs one fetch unless one is already in flight, in which
// case it waits for that one. A forced refresh waits out an unforced fetch
// and then makes its own.
func (m *PlanUsageMonitor) refresh(ctx context.Context, forced bool) {
	m.mu.Lock()
	for m.inflight {
		ch := make(chan struct{})
		m.waiters = append(m.waiters, ch)
		joinable := m.inflightForced || !forced
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
		if joinable {
			return
		}
		m.mu.Lock()
	}
	m.inflight = true
	m.inflightForced = forced
	m.mu.Unlock()

	fetch := m.fetch
	if forced {
		fetch = m.force
	}
	fetched, err := fetch(ctx)

	m.mu.Lock()
	m.inflight = false
	m.inflightForced = false
	if err == nil {
		m.snapshot = fetched
		m.fetchedAt = m.clock.Now()
		m.lastErr = ""
	} else {
		m.lastErr = err.Error()
	}
	waiters := m.waiters
	m.waiters = nil
	m.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	if m.onUpdate != nil {
		m.onUpdate(fetched, err)
	}
}

// Invalidate asks for an early refresh: a seat reported a rate limit or a
// quota stop, so the snapshot is wrong until re-read. It also makes the
// next Read refetch, whether or not Run is looping.
func (m *PlanUsageMonitor) Invalidate() {
	m.mu.Lock()
	m.fetchedAt = time.Time{}
	m.mu.Unlock()
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Read returns the snapshot, refreshing first when force is set or when it
// is older than the TTL. A snapshot that has never been fetched and cannot
// be fetched now has a zero FetchedAt and the fetch error in Err.
func (m *PlanUsageMonitor) Read(ctx context.Context, force bool) PlanUsageSnapshot {
	m.mu.Lock()
	stale := m.fetchedAt.IsZero() || m.clock.Now().Sub(m.fetchedAt) > m.ttl
	m.mu.Unlock()
	if force || stale {
		m.refresh(ctx, force)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return PlanUsageSnapshot{
		Backends:  append([]PlanUsage(nil), m.snapshot...),
		FetchedAt: m.fetchedAt,
		Err:       m.lastErr,
	}
}

// The running monitors in this process, newest last.
var (
	planMonitorsMu sync.Mutex
	planMonitors   []*PlanUsageMonitor
)

func registerPlanUsageMonitor(m *PlanUsageMonitor) (unregister func()) {
	planMonitorsMu.Lock()
	planMonitors = append(planMonitors, m)
	planMonitorsMu.Unlock()
	return func() {
		planMonitorsMu.Lock()
		defer planMonitorsMu.Unlock()
		for i, x := range planMonitors {
			if x == m {
				planMonitors = append(planMonitors[:i], planMonitors[i+1:]...)
				return
			}
		}
	}
}

// runningPlanUsageMonitor is the monitor LoadPlanUsage reads, or nil.
func runningPlanUsageMonitor() *PlanUsageMonitor {
	planMonitorsMu.Lock()
	defer planMonitorsMu.Unlock()
	if len(planMonitors) == 0 {
		return nil
	}
	return planMonitors[len(planMonitors)-1]
}

// invalidatePlanUsage is what a rate-limit or quota stop on an in-process
// agent does to the snapshots this host reads: every running monitor
// refetches, and the default filesystem cache is marked stale so the next
// LoadPlanUsage in any brokerless process fetches rather than waiting out
// the TTL.
func invalidatePlanUsage() {
	planMonitorsMu.Lock()
	monitors := append([]*PlanUsageMonitor(nil), planMonitors...)
	planMonitorsMu.Unlock()
	for _, m := range monitors {
		m.Invalidate()
	}
	markDefaultPlanCacheStale()
}

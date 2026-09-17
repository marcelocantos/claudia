// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// startInvalidatingAgent is a direct-mode agent a test can make report a
// rate limit.
func startInvalidatingAgent(t *testing.T) *Agent {
	t.Helper()
	a, err := startWithBackend(Config{WorkDir: t.TempDir(), SessionID: "sid-429", TermLogPath: "-"},
		&fakeAgentBackend{name: "fake-claude"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	return a
}

var rateLimited = Event{Type: "result", IsError: true, Text: "API Error: 429 rate_limit_error"}

// TestDirectRateLimitInvalidatesFileCache (🎯T75.4): with no daemon and no
// monitor, a rate limit on an in-process agent makes the next LoadPlanUsage
// fetch again inside the TTL instead of serving the snapshot it just proved
// wrong.
func TestDirectRateLimitInvalidatesFileCache(t *testing.T) {
	t.Setenv("CLAUDIA_PLAN_CACHE", t.TempDir())
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	var fetches atomic.Int32
	load := func() {
		t.Helper()
		// Dir stays empty: the default cache is the one a stop invalidates.
		// Fetch is injected, which keeps the read off the daemon socket.
		if _, err := LoadPlanUsage(context.Background(), &PlanUsageCacheArgs{
			Now: now,
			Fetch: func(context.Context) ([]PlanUsage, error) {
				fetches.Add(1)
				return []PlanUsage{{Provider: ProviderClaude}}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	load()
	load()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches before the stop = %d, want 1 (cache hit)", got)
	}
	startInvalidatingAgent(t).PublishEvent(rateLimited)
	load()
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches after a rate limit = %d, want 2", got)
	}
	load()
	if got := fetches.Load(); got != 2 {
		t.Fatalf("the refetched snapshot is not current: fetches = %d", got)
	}
}

// TestRunningMonitorIsTheInProcessEvaluator (🎯T75.4): while a monitor
// runs, LoadPlanUsage with default arguments reads it, and a rate limit on
// an in-process agent makes it refetch without the clock moving.
func TestRunningMonitorIsTheInProcessEvaluator(t *testing.T) {
	clock := NewManualClock(time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC))
	var fetches atomic.Int32
	m := NewPlanUsageMonitor(&PlanUsageMonitorArgs{
		Clock: clock,
		Fetch: func(context.Context) ([]PlanUsage, error) {
			fetches.Add(1)
			return []PlanUsage{{Provider: ProviderCodex}}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "first fetch", func() bool { return fetches.Load() == 1 && runningPlanUsageMonitor() == m })

	got, err := LoadPlanUsage(context.Background(), nil)
	if err != nil || len(got) != 1 || got[0].Provider != ProviderCodex {
		t.Fatalf("LoadPlanUsage = %+v, %v; want the monitor's snapshot", got, err)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("reading a current monitor fetched again: %d", n)
	}

	startInvalidatingAgent(t).PublishEvent(rateLimited)
	waitFor(t, "refetch after rate limit", func() bool { return fetches.Load() == 2 })

	cancel()
	<-done
	if runningPlanUsageMonitor() == m {
		t.Fatal("a stopped monitor is still the evaluator")
	}
}

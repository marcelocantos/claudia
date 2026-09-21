// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func TestLoadPlanUsageFreshHitSkipsFetch(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if err := writePlanSnapshot(dir+"/"+planCacheSnapshotFile, planCacheSnapshot{
		FetchedAt: now,
		Backends:  []PlanUsage{{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "cached", FetchedAt: now}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPlanUsage(context.Background(), &PlanUsageCacheArgs{
		Dir: dir,
		TTL: time.Hour,
		Now: now.Add(time.Minute),
		Fetch: func(context.Context) ([]PlanUsage, error) {
			t.Fatal("fetch must not run on a fresh hit")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "cached" {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadPlanUsageSingleFetchUnderContention(t *testing.T) {
	dir := t.TempDir()
	var fetches atomic.Int32
	gate := make(chan struct{})
	fetch := func(ctx context.Context) ([]PlanUsage, error) {
		fetches.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []PlanUsage{{Provider: ProviderClaude, Status: PlanUsageUnavailable, Reason: "once"}}, nil
	}
	ctx := wallclockguard.UntilTestTimeout(t)
	args := func() *PlanUsageCacheArgs {
		return &PlanUsageCacheArgs{
			Dir:       dir,
			TTL:       time.Hour,
			LockStale: 10 * time.Second, // long enough that a slow fetch under load is never stolen
			Poll:      5 * time.Millisecond,
			Fetch:     fetch,
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var a, b []PlanUsage
	var errA, errB error
	go func() {
		defer wg.Done()
		a, errA = LoadPlanUsage(ctx, args())
	}()
	go func() {
		defer wg.Done()
		b, errB = LoadPlanUsage(ctx, args())
	}()
	deadline := time.Now().Add(2 * time.Second)
	for fetches.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("holder never started a fetch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("errs %v %v", errA, errB)
	}
	if fetches.Load() != 1 {
		t.Fatalf("fetches=%d want 1", fetches.Load())
	}
	if len(a) != 1 || len(b) != 1 || a[0].Reason != "once" || b[0].Reason != "once" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestLoadPlanUsageStealsQuietLease(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	lease := dir + "/" + planCacheLockFile
	if err := writePlanLease(lease, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	got, err := LoadPlanUsage(context.Background(), &PlanUsageCacheArgs{
		Dir:       dir,
		TTL:       time.Hour,
		LockStale: 20 * time.Millisecond,
		Poll:      5 * time.Millisecond,
		Now:       now,
		Fetch: func(context.Context) ([]PlanUsage, error) {
			fetches.Add(1)
			return []PlanUsage{{Provider: ProviderCodex, Status: PlanUsageUnavailable, Reason: "stolen"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("fetches=%d", fetches.Load())
	}
	if len(got) != 1 || got[0].Reason != "stolen" {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadPlanUsageRefreshBypassesFreshHit(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 15, 7, 0, 0, 0, time.UTC)
	if err := writePlanSnapshot(dir+"/"+planCacheSnapshotFile, planCacheSnapshot{
		FetchedAt: now,
		Backends:  []PlanUsage{{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "401", FetchedAt: now}},
	}); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	got, err := LoadPlanUsage(context.Background(), &PlanUsageCacheArgs{
		Dir:     dir,
		TTL:     time.Hour,
		Now:     now.Add(time.Minute),
		Refresh: true,
		Fetch: func(context.Context) ([]PlanUsage, error) {
			fetches.Add(1)
			return []PlanUsage{{
				Provider: ProviderGrok, Status: PlanUsageAvailable, Reason: "after login", FetchedAt: now.Add(time.Minute),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("refresh must fetch, fetches=%d", fetches.Load())
	}
	if len(got) != 1 || got[0].Reason != "after login" {
		t.Fatalf("got %+v", got)
	}
}

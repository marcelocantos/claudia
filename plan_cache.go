// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

const (
	planCacheSnapshotFile = "snapshot.json"
	planCacheLockFile     = "lock.json"
	planCacheFlockFile    = "lock.flock"

	// DefaultPlanCacheTTL is how long a snapshot is treated as current.
	DefaultPlanCacheTTL = 5 * time.Minute
	// DefaultPlanCacheLockStale is how long a quiet lock holder may sit
	// before waiters steal the lease (🎯T61.2).
	DefaultPlanCacheLockStale = 20 * time.Second
	// DefaultPlanCachePoll is the waiter backoff while the lock is held.
	DefaultPlanCachePoll = 50 * time.Millisecond
)

// PlanUsageCacheArgs configures [LoadPlanUsage].
type PlanUsageCacheArgs struct {
	// Dir is the cache directory. Empty uses the user cache dir
	// (`…/claudia/plan-usage`) or CLAUDIA_PLAN_CACHE.
	Dir string
	// TTL is how long a snapshot is fresh. Zero uses DefaultPlanCacheTTL.
	TTL time.Duration
	// LockStale is the quiet-holder steal timeout. Zero uses DefaultPlanCacheLockStale.
	LockStale time.Duration
	// Poll is the waiter interval. Zero uses DefaultPlanCachePoll.
	Poll time.Duration
	// Now overrides the clock (tests).
	Now time.Time
	// Fetch replaces QueryAllPlanUsage (tests). Nil uses All.
	Fetch func(ctx context.Context) ([]PlanUsage, error)
	// All is passed to QueryAllPlanUsage when Fetch is nil.
	All *AllPlanUsageArgs
}

type planCacheSnapshot struct {
	FetchedAt time.Time   `json:"fetched_at"`
	Backends  []PlanUsage `json:"backends"`
}

type planCacheLease struct {
	PID       int       `json:"pid"`
	Heartbeat time.Time `json:"heartbeat"`
}

// LoadPlanUsage returns a host-shared plan-usage snapshot (🎯T61.2).
// Fresh cache hits do not call vendor endpoints. A miss takes an exclusive
// lease, rechecks the snapshot, then fetches and writes before releasing
// so a waiter cannot start a second fetch in the publish window. Waiters
// poll until the lease is released or goes quiet (heartbeat older than
// LockStale), then either steal or read the new snapshot.
func LoadPlanUsage(ctx context.Context, args *PlanUsageCacheArgs) ([]PlanUsage, error) {
	if args == nil {
		args = &PlanUsageCacheArgs{}
	}
	// A listening daemon is the one evaluator on the host (🎯T2.9); the
	// filesystem cache below is the brokerless path. A test that injects
	// Fetch is asking for the cache and never reaches the socket.
	if args.Fetch == nil && args.Dir == "" {
		usage, _, err := brokerUsage(ctx, false)
		if err == nil {
			return usage, nil
		}
		if !brokerFellThrough(err) {
			return nil, err
		}
	}
	ttl := args.TTL
	if ttl <= 0 {
		ttl = DefaultPlanCacheTTL
	}
	stale := args.LockStale
	if stale <= 0 {
		stale = DefaultPlanCacheLockStale
	}
	poll := args.Poll
	if poll <= 0 {
		poll = DefaultPlanCachePoll
	}
	now := args.Now
	if now.IsZero() {
		now = time.Now()
	}
	dir, err := planCacheDir(args.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("plan cache: %w", err)
	}
	snapPath := filepath.Join(dir, planCacheSnapshotFile)
	leasePath := filepath.Join(dir, planCacheLockFile)
	flockPath := filepath.Join(dir, planCacheFlockFile)

	if snap, ok := readFreshPlanSnapshot(snapPath, now, ttl); ok {
		return snap.Backends, nil
	}

	fetch := args.Fetch
	if fetch == nil {
		all := args.All
		if all == nil {
			all = &AllPlanUsageArgs{Now: now}
		} else if all.Now.IsZero() {
			cp := *all
			cp.Now = now
			all = &cp
		}
		fetch = func(ctx context.Context) ([]PlanUsage, error) {
			return QueryAllPlanUsage(ctx, all)
		}
	}

	deadline, hasDeadline := ctx.Deadline()
	for {
		if err := ctx.Err(); err != nil {
			if snap, err2 := readPlanSnapshot(snapPath); err2 == nil && snap != nil {
				return snap.Backends, nil
			}
			return nil, err
		}
		if snap, ok := readFreshPlanSnapshot(snapPath, clockNow(args.Now), ttl); ok {
			return snap.Backends, nil
		}

		held, err := tryHoldPlanLease(leasePath, flockPath, stale, clockNow(args.Now))
		if err != nil {
			return nil, err
		}
		if held != nil {
			// Recheck under the lease. A sibling can publish and
			// release in the window between the miss above and
			// tryHoldPlanLease; fetching again would break
			// single-flight (🎯T65).
			if snap, ok := readFreshPlanSnapshot(snapPath, clockNow(args.Now), ttl); ok {
				held.release()
				return snap.Backends, nil
			}
			backends, ferr := fetch(ctx)
			writePlanLeaseHeartbeat(leasePath, clockNow(args.Now))
			if ferr != nil {
				held.release()
				if snap, err2 := readPlanSnapshot(snapPath); err2 == nil && snap != nil {
					return snap.Backends, ferr
				}
				return nil, ferr
			}
			doc := planCacheSnapshot{FetchedAt: clockNow(args.Now), Backends: backends}
			err := writePlanSnapshot(snapPath, doc)
			held.release()
			if err != nil {
				return backends, err
			}
			return backends, nil
		}

		if hasDeadline && clockNow(args.Now).After(deadline) {
			if snap, err2 := readPlanSnapshot(snapPath); err2 == nil && snap != nil {
				return snap.Backends, nil
			}
			return nil, context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			if snap, err2 := readPlanSnapshot(snapPath); err2 == nil && snap != nil {
				return snap.Backends, nil
			}
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func planCacheDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("CLAUDIA_PLAN_CACHE"); env != "" {
		return env, nil
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("plan cache: %w", err)
	}
	return filepath.Join(root, "claudia", "plan-usage"), nil
}

func clockNow(override time.Time) time.Time {
	if !override.IsZero() {
		return override
	}
	return time.Now()
}

func readFreshPlanSnapshot(path string, now time.Time, ttl time.Duration) (*planCacheSnapshot, bool) {
	snap, err := readPlanSnapshot(path)
	if err != nil || snap == nil {
		return nil, false
	}
	if now.Sub(snap.FetchedAt) > ttl {
		return nil, false
	}
	return snap, true
}

func readPlanSnapshot(path string) (*planCacheSnapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc planCacheSnapshot
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("plan cache snapshot: %w", err)
	}
	return &doc, nil
}

func writePlanSnapshot(path string, doc planCacheSnapshot) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Unique tmp so two overlapping writers (lease steal) cannot
	// rename each other's snapshot.json.tmp out from under them.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

type planLeaseHold struct {
	flock *os.File
	path  string
	alive *int32
}

func (h *planLeaseHold) release() {
	if h == nil {
		return
	}
	if h.alive != nil {
		atomic.StoreInt32(h.alive, 0)
	}
	if h.flock != nil {
		_ = flockUnlock(h.flock)
		_ = h.flock.Close()
	}
	_ = os.Remove(h.path)
}

func tryHoldPlanLease(leasePath, flockPath string, stale time.Duration, now time.Time) (*planLeaseHold, error) {
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o700); err != nil {
		return nil, err
	}
	ff, err := os.OpenFile(flockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("plan cache flock: %w", err)
	}
	ok, err := flockTryExclusive(ff)
	if err != nil {
		_ = ff.Close()
		return nil, err
	}
	if !ok {
		_ = ff.Close()
		if leaseQuiet(leasePath, stale, now) {
			_ = os.Remove(leasePath)
		}
		return nil, nil
	}

	if !leaseQuiet(leasePath, stale, now) && leaseExists(leasePath) {
		_ = flockUnlock(ff)
		_ = ff.Close()
		return nil, nil
	}

	if err := writePlanLease(leasePath, now); err != nil {
		_ = flockUnlock(ff)
		_ = ff.Close()
		return nil, err
	}
	hold := &planLeaseHold{flock: ff, path: leasePath, alive: new(int32)}
	atomic.StoreInt32(hold.alive, 1)
	go heartbeatPlanLease(leasePath, hold.alive)
	return hold, nil
}

func heartbeatPlanLease(path string, alive *int32) {
	ticker := time.NewTicker(DefaultPlanCacheLockStale / 4)
	defer ticker.Stop()
	for range ticker.C {
		if atomic.LoadInt32(alive) == 0 {
			return
		}
		writePlanLeaseHeartbeat(path, time.Now())
	}
}

func writePlanLease(path string, now time.Time) error {
	doc := planCacheLease{PID: os.Getpid(), Heartbeat: now}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func writePlanLeaseHeartbeat(path string, now time.Time) {
	_ = writePlanLease(path, now)
}

func leaseExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func leaseQuiet(path string, stale time.Duration, now time.Time) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	var doc planCacheLease
	if err := json.Unmarshal(raw, &doc); err != nil {
		return true
	}
	if doc.Heartbeat.IsZero() {
		return true
	}
	return now.Sub(doc.Heartbeat) > stale
}

// errNoPlanCache is reserved for tests that want a distinct miss.
var errNoPlanCache = errors.New("plan cache empty")

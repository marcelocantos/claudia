// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/daemonproto"
)

// livePIDs is a test helper that treats a fixed set of PIDs as alive.
func livePIDs(alive ...int) func(int) bool {
	set := make(map[int]bool, len(alive))
	for _, p := range alive {
		set[p] = true
	}
	return func(pid int) bool { return set[pid] }
}

func TestRegisterThenObserveAppendsChain(t *testing.T) {
	s := NewState(WithPIDAliveFn(livePIDs(100)))

	s.Register("/tmp/work", "sid-a", 100)

	chain, conf, ok := s.LookupChain("sid-a")
	if !ok || len(chain) != 1 || chain[0] != "sid-a" {
		t.Fatalf("after register, expected chain [sid-a], got %v (ok=%v)", chain, ok)
	}
	if conf != daemonproto.ConfidenceDeterministic {
		t.Fatalf("initial confidence should be deterministic, got %q", conf)
	}

	s.ObserveEscaped(EscapeCwd("/tmp/work"), "sid-b")

	chain, conf, ok = s.LookupChain("sid-b")
	if !ok || len(chain) != 2 || chain[0] != "sid-a" || chain[1] != "sid-b" {
		t.Fatalf("after rollover, expected chain [sid-a sid-b], got %v (ok=%v)", chain, ok)
	}
	if conf != daemonproto.ConfidenceDeterministic {
		t.Fatalf("single-pid attribution should be deterministic, got %q", conf)
	}

	// Looking up the original sid should still return the full chain.
	chain, _, _ = s.LookupChain("sid-a")
	if len(chain) != 2 {
		t.Fatalf("lookup of sid-a should see the full chain, got %v", chain)
	}
}

func TestObserveBeforeRegisterReconciles(t *testing.T) {
	now := time.Now()
	s := NewState(
		WithPIDAliveFn(livePIDs(42)),
		WithNowFn(func() time.Time { return now }),
		WithPendingTTL(time.Second),
	)

	// Observation arrives before Register — goes into pending.
	s.ObserveEscaped(EscapeCwd("/tmp/w"), "sid-orphan")
	if _, _, ok := s.LookupChain("sid-orphan"); ok {
		t.Fatalf("orphan observation should not produce a chain yet")
	}

	// Register then observes the pending sid via replay.
	s.Register("/tmp/w", "sid-head", 42)

	chain, _, ok := s.LookupChain("sid-orphan")
	if !ok {
		t.Fatalf("after register, pending sid should be attributed")
	}
	if len(chain) != 2 || chain[0] != "sid-head" || chain[1] != "sid-orphan" {
		t.Fatalf("expected chain [sid-head sid-orphan], got %v", chain)
	}
}

func TestObserveOrphanExpires(t *testing.T) {
	clock := time.Now()
	tick := func(d time.Duration) { clock = clock.Add(d) }
	s := NewState(
		WithPIDAliveFn(livePIDs()),
		WithNowFn(func() time.Time { return clock }),
		WithPendingTTL(100*time.Millisecond),
	)

	s.ObserveEscaped(EscapeCwd("/tmp/w"), "sid-orphan")

	// Not yet expired.
	s.Sweep()
	s.mu.Lock()
	if len(s.pending) != 1 {
		s.mu.Unlock()
		t.Fatalf("pending should still contain 1 entry before TTL")
	}
	s.mu.Unlock()

	tick(200 * time.Millisecond)
	s.Sweep()

	s.mu.Lock()
	if len(s.pending) != 0 {
		s.mu.Unlock()
		t.Fatalf("pending should be empty after TTL")
	}
	s.mu.Unlock()
}

func TestAmbiguousAttributionMarksChain(t *testing.T) {
	s := NewState(WithPIDAliveFn(livePIDs(100, 200)))

	s.Register("/tmp/same", "sid-a", 100)
	s.Register("/tmp/same", "sid-b", 200)

	// A new jsonl appears with neither PID clearly owning it.
	s.ObserveEscaped(EscapeCwd("/tmp/same"), "sid-c")

	chain, conf, ok := s.LookupChain("sid-c")
	if !ok {
		t.Fatalf("sid-c should be attributed to one of the chains")
	}
	if len(chain) != 2 {
		t.Fatalf("expected 2-element chain, got %v", chain)
	}
	if conf != daemonproto.ConfidenceAmbiguous {
		t.Fatalf("multi-pid attribution should flag ambiguity, got %q", conf)
	}
}

func TestDeadPIDGetsPrunedOnObserve(t *testing.T) {
	alive := map[int]bool{100: true}
	s := NewState(WithPIDAliveFn(func(pid int) bool { return alive[pid] }))

	s.Register("/tmp/w", "sid-a", 100)

	// Simulate the owner dying.
	alive[100] = false

	// Subsequent observation can't be attributed.
	s.ObserveEscaped(EscapeCwd("/tmp/w"), "sid-b")
	if _, _, ok := s.LookupChain("sid-b"); ok {
		t.Fatalf("observation after owner death should not attribute")
	}

	s.mu.Lock()
	if _, ok := s.pidInfo[100]; ok {
		s.mu.Unlock()
		t.Fatalf("dead pid should have been pruned")
	}
	s.mu.Unlock()
}

func TestRegisterIsIdempotent(t *testing.T) {
	s := NewState(WithPIDAliveFn(livePIDs(1)))
	s.Register("/tmp/w", "sid-a", 1)
	s.Register("/tmp/w", "sid-a", 1) // exact re-register
	s.Register("/tmp/w", "sid-x", 1) // conflict: different sid, same pid
	s.ObserveEscaped(EscapeCwd("/tmp/w"), "sid-b")

	chain, _, ok := s.LookupChain("sid-b")
	if !ok || len(chain) != 2 || chain[0] != "sid-a" {
		t.Fatalf("chain should still originate from sid-a, got %v", chain)
	}
}

func TestSubscribeReceivesChainUpdate(t *testing.T) {
	s := NewState(WithPIDAliveFn(livePIDs(100)))

	id, ch := s.Subscribe()
	if id == "" {
		t.Fatalf("subscription id must be non-empty")
	}

	s.Register("/tmp/w", "sid-a", 100)

	// Register alone doesn't emit a chain.update (the chain was seeded,
	// not extended). The first notification comes from Observe.
	select {
	case u := <-ch:
		t.Fatalf("unexpected update after register: %+v", u)
	case <-time.After(10 * time.Millisecond):
	}

	s.ObserveEscaped(EscapeCwd("/tmp/w"), "sid-b")

	select {
	case u := <-ch:
		if len(u.Chain) != 2 || u.Chain[1] != "sid-b" {
			t.Fatalf("unexpected update chain: %v", u.Chain)
		}
		if u.SubscriptionID != id {
			t.Fatalf("expected subscription id %q, got %q", id, u.SubscriptionID)
		}
	case <-time.After(time.Second):
		t.Fatalf("expected a chain.update notification")
	}

	s.Unsubscribe(id)
	// Channel must close after unsubscribe.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("channel should be closed after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatalf("channel not closed after unsubscribe")
	}
}

func TestConcurrentRegisterAndObserve(t *testing.T) {
	// Sanity check for races under -race.
	s := NewState(WithPIDAliveFn(func(int) bool { return true }))

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		i := i
		go func() {
			defer wg.Done()
			s.Register("/tmp/w", "sid-r-"+intToStr(i), 1000+i)
		}()
		go func() {
			defer wg.Done()
			s.ObserveEscaped(EscapeCwd("/tmp/w"), "sid-o-"+intToStr(i))
		}()
	}
	wg.Wait()
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

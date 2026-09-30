// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// 🎯T161: after a broker restart the fleet came back one or two seats every
// 60-90 s. The two resume slots went to seats in name order — old workers
// nobody had claimed in days ahead of the product owners a consumer was
// waiting on — and a relaunched seat held its slot while its restart nudge
// waited for the seat to be ready.

// TestT161ResumeNudgeDoesNotHoldAStartSlot: with one slot, a seat whose
// nudge cannot be delivered yet does not keep the next seat from starting.
func TestT161ResumeNudgeDoesNotHoldAStartSlot(t *testing.T) {
	f := newSeatFixture(t)
	unblock := make(chan struct{})
	var mu sync.Mutex
	var started []string
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		mu.Lock()
		started = append(started, cfg.Name)
		mu.Unlock()
		send := func(string) error { return nil }
		if cfg.Name == "a-slow-to-ready" {
			// A Claude seat's Send waits for its composer.
			send = func(string) error { <-unblock; return nil }
		}
		return StartStub(ctx, cfg, &StubAgentOps{Send: send})
	}
	f.open(t, []AgentDef{
		{Name: "a-slow-to-ready", WorkDir: t.TempDir(), SessionID: "sid-a", AutoStart: true},
		{Name: "b-next", WorkDir: t.TempDir(), SessionID: "sid-b", AutoStart: true},
	})
	done := make(chan []ResumeOutcome, 1)
	go func() {
		done <- f.reg.ResumeAll(context.Background(), &ResumeArgs{Nudge: "restart-nudge", Concurrency: 1})
	}()
	waitFor(t, "b-next started while a-slow-to-ready's nudge is still waiting", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == 2
	})
	close(unblock)
	for _, o := range <-done {
		if o.Err != nil || o.NudgeErr != nil || !o.Nudged {
			t.Fatalf("%s: %+v", o.Name, o)
		}
	}
}

// TestT161ResumeSelectOrdersAndLeavesOut: Select decides which AutoStart
// seats come back and in what order. A seat it leaves out is not started;
// a name that is not an AutoStart seat, or is named twice, starts nothing
// extra.
func TestT161ResumeSelectOrdersAndLeavesOut(t *testing.T) {
	f := newSeatFixture(t)
	var mu sync.Mutex
	var started []string
	launch := registryStart
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		mu.Lock()
		started = append(started, cfg.Name)
		mu.Unlock()
		return launch(ctx, cfg)
	}
	var defs []AgentDef
	for _, n := range []string{"a-old-worker", "b-old-worker", "jevons-po", "zz-po"} {
		defs = append(defs, AgentDef{Name: n, WorkDir: t.TempDir(), SessionID: "sid-" + n, AutoStart: true})
	}
	defs = append(defs, AgentDef{Name: "idle", WorkDir: t.TempDir(), SessionID: "sid-idle"})
	f.open(t, defs)

	var offered []string
	out := f.reg.ResumeAll(context.Background(), &ResumeArgs{
		Nudge:       NoRestartNudge,
		Concurrency: 1,
		Select: func(names []string) []string {
			offered = names
			return []string{"zz-po", "idle", "jevons-po", "zz-po", "b-old-worker"}
		},
	})
	if got := strings.Join(offered, ","); got != "a-old-worker,b-old-worker,jevons-po,zz-po" {
		t.Fatalf("Select was offered %s, want the AutoStart seats in name order", got)
	}
	if got := strings.Join(started, ","); got != "zz-po,jevons-po,b-old-worker" {
		t.Fatalf("starts = %s, want zz-po,jevons-po,b-old-worker", got)
	}
	var names []string
	for _, o := range out {
		if o.Err != nil {
			t.Fatalf("%s: %v", o.Name, o.Err)
		}
		names = append(names, o.Name)
	}
	if got := strings.Join(names, ","); got != "zz-po,jevons-po,b-old-worker" {
		t.Fatalf("outcomes = %s, want the selected order", got)
	}
}

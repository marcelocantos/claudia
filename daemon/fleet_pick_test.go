// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestGrantPickByRemainingKeepsProviderOnReclaim(t *testing.T) {
	var current atomic.Value
	current.Store(fleetSnap(t, 10, 90, 40, 20))
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume = true
	opts.UsageFetch = func(context.Context) ([]claudia.PlanUsage, error) {
		f.mu.Lock()
		f.fetches++
		f.mu.Unlock()
		return current.Load().([]claudia.PlanUsage), nil
	}
	f.bootWith(t, opts)
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })

	name := "pick-seat"
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: name, WorkDir: t.TempDir(), SessionID: "sid-pick", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	resp := rawCall(t, c, &broker.Request{ID: "g1", Type: broker.TypeGrant, Grant: &broker.GrantRequest{
		Name: name, Def: raw, Pick: claudia.PickRemaining,
	}})
	if resp.Type != broker.TypeGranted {
		t.Fatalf("grant answered %s: %+v", resp.Type, resp.Error)
	}
	if resp.Granted.Provider != broker.Provider(claudia.ProviderGrok) {
		t.Fatalf("provider = %s, want grok", resp.Granted.Provider)
	}
	if resp.Granted.RemainingPercent == nil || *resp.Granted.RemainingPercent != 90 {
		t.Fatalf("remaining = %v, want 90", resp.Granted.RemainingPercent)
	}
	if stored := f.d.reg.Def(name); stored == nil || stored.Provider != claudia.ProviderGrok {
		t.Fatalf("stored provider = %+v", stored)
	}

	// Cursor is now the fullest. A reclaim must keep the grok seat.
	current.Store(fleetSnap(t, 99, 1, 1, 1))
	f.clock.Advance(claudia.DefaultPlanCacheTTL + time.Second)
	resp = rawCall(t, c, &broker.Request{ID: "g2", Type: broker.TypeGrant, Grant: &broker.GrantRequest{
		Name: name, Def: raw, Pick: claudia.PickRemaining,
	}})
	if resp.Type != broker.TypeGranted {
		t.Fatalf("reclaim answered %s: %+v", resp.Type, resp.Error)
	}
	if !resp.Granted.Reclaimed || resp.Granted.Provider != broker.Provider(claudia.ProviderGrok) {
		t.Fatalf("reclaim = reclaimed:%v provider:%s", resp.Granted.Reclaimed, resp.Granted.Provider)
	}
	if resp.Granted.RemainingPercent != nil {
		t.Fatalf("reclaim reported a fresh remaining percent: %v", *resp.Granted.RemainingPercent)
	}
	if stored := f.d.reg.Def(name); stored == nil || stored.Provider != claudia.ProviderGrok {
		t.Fatalf("stored provider after reclaim = %+v", stored)
	}
	if f.seatCount() != 1 {
		t.Fatalf("seats started = %d, want 1", f.seatCount())
	}
}

func fleetSnap(t *testing.T, cursor, grok, claude, codex float64) []claudia.PlanUsage {
	t.Helper()
	row := func(p claudia.Provider, rem float64) claudia.PlanUsage {
		pct := rem
		return claudia.PlanUsage{
			Provider: p,
			Status:   claudia.PlanUsageAvailable,
			Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &pct}},
		}
	}
	return []claudia.PlanUsage{
		row(claudia.ProviderCursor, cursor),
		row(claudia.ProviderGrok, grok),
		row(claudia.ProviderClaude, claude),
		row(claudia.ProviderCodex, codex),
	}
}

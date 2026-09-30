// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)

// TestRunBrokerTaskStreamsAndAdmits: RunBrokerTask is task_run on the
// daemon. A provider with usable capacity is spawned only after the usage
// snapshot has been read, and that snapshot is what LoadPlanUsage returns.
func TestRunBrokerTaskStreamsAndAdmits(t *testing.T) {
	pct := 40.0
	usage := []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &pct}},
	}}
	f := newFixture(t)
	f.boot(t, usage)
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })

	var spawned int
	prev := daemonNewTask
	daemonNewTask = func(cfg claudia.TaskConfig) *claudia.Task {
		if f.fetchCount() < 1 {
			t.Errorf("spawned before a usage read: fetches=%d", f.fetchCount())
		}
		spawned++
		if cfg.Provider != claudia.ProviderGrok {
			t.Errorf("provider = %q", cfg.Provider)
		}
		return claudia.NewStubTask(cfg, &claudia.StubTaskOps{Run: func(_ context.Context, run claudia.StubTaskRun) (<-chan claudia.TaskEvent, error) {
			if run.Prompt != "ping" {
				t.Errorf("prompt = %q", run.Prompt)
			}
			ch := make(chan claudia.TaskEvent, 2)
			ch <- claudia.TaskEvent{Type: claudia.TaskEventText, Content: "pong"}
			ch <- claudia.TaskEvent{Type: claudia.TaskEventResult, Content: "pong"}
			close(ch)
			return ch, nil
		}})
	}
	t.Cleanup(func() { daemonNewTask = prev })

	// Claude is the only row, and it admits. Grok has no row, so it is
	// admitted too: a missing reading is not a veto.
	ch, err := claudia.RunBrokerTask(context.Background(), "ping", claudia.TaskConfig{
		Provider: claudia.ProviderGrok, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []claudia.TaskEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].Content != "pong" || got[1].Type != claudia.TaskEventResult {
		t.Fatalf("events = %+v", got)
	}
	if spawned != 1 {
		t.Fatalf("spawned %d times", spawned)
	}

	snap, err := claudia.LoadPlanUsage(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var claude claudia.PlanUsage
	for _, u := range snap {
		if u.Provider == claudia.ProviderClaude {
			claude = u
		}
	}
	if !claudia.HasAvailableTokens(claude, f.clock.Now(), nil) {
		t.Fatalf("usage snapshot does not admit claude: %+v", claude)
	}
}

// TestRunBrokerTaskRefusesExhaustedPlan: a published exhaustion is
// plan_exhausted, the stub is never started, and LoadPlanUsage still
// shows the snapshot that refused the run.
func TestRunBrokerTaskRefusesExhaustedPlan(t *testing.T) {
	zero := 0.0
	usage := []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &zero}},
	}}
	f := newFixture(t)
	f.boot(t, usage)
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })

	var spawned int
	prev := daemonNewTask
	daemonNewTask = func(cfg claudia.TaskConfig) *claudia.Task {
		spawned++
		return claudia.NewStubTask(cfg, nil)
	}
	t.Cleanup(func() { daemonNewTask = prev })

	for _, provider := range []claudia.Provider{claudia.ProviderClaude, ""} {
		_, err := claudia.RunBrokerTask(context.Background(), "ping", claudia.TaskConfig{
			Provider: provider, WorkDir: t.TempDir(),
		})
		if !errors.Is(err, claudia.ErrPlanExhausted) {
			t.Fatalf("provider %q: %v", provider, err)
		}
	}
	if spawned != 0 {
		t.Fatalf("exhausted plan spawned %d tasks", spawned)
	}

	snap, err := claudia.LoadPlanUsage(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var claude claudia.PlanUsage
	for _, u := range snap {
		if u.Provider == claudia.ProviderClaude {
			claude = u
		}
	}
	if claudia.HasAvailableTokens(claude, f.clock.Now(), nil) {
		t.Fatalf("usage snapshot still admits claude: %+v", claude)
	}
}

// TestRunBrokerTaskRefreshesUsageBeforeSpawn: a stale snapshot is re-read
// before the provider is started, so admission and `claudia broker usage`
// see the same fetch.
func TestRunBrokerTaskRefreshesUsageBeforeSpawn(t *testing.T) {
	pct := 40.0
	usage := []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &pct}},
	}}
	f := newFixture(t)
	f.boot(t, usage)
	waitFor(t, "boot fetch", func() bool { return f.fetchCount() >= 1 })
	f.clock.Advance(claudia.DefaultPlanCacheTTL + time.Second)

	prev := daemonNewTask
	daemonNewTask = func(cfg claudia.TaskConfig) *claudia.Task {
		if f.fetchCount() < 2 {
			t.Errorf("spawned on the stale snapshot: fetches=%d", f.fetchCount())
		}
		return claudia.NewStubTask(cfg, &claudia.StubTaskOps{Run: func(context.Context, claudia.StubTaskRun) (<-chan claudia.TaskEvent, error) {
			ch := make(chan claudia.TaskEvent, 1)
			ch <- claudia.TaskEvent{Type: claudia.TaskEventResult, Content: "ok"}
			close(ch)
			return ch, nil
		}})
	}
	t.Cleanup(func() { daemonNewTask = prev })

	ch, err := claudia.RunBrokerTask(context.Background(), "ping", claudia.TaskConfig{
		Provider: claudia.ProviderClaude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if f.fetchCount() < 2 {
		t.Fatalf("admission did not refresh usage: fetches=%d", f.fetchCount())
	}
}

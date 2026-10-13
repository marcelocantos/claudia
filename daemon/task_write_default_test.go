// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
)

// Colossus, 2026-10-12: `claudia broker task --pick remaining` chose codex
// and the seat ran read-only. The daemon gives the picked codex task the
// workspace-write sandbox a claude or grok seat effectively has.
func TestTaskPickRemainingCodexGetsWriteSandbox(t *testing.T) {
	f := newFixture(t)
	opts := f.options(fleetSnap(t, 15, 40, 30, 99))
	opts.DisableResume = true
	opts.resolveBinary = func(claudia.Provider, bool) error { return nil }
	f.bootWith(t, opts)
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })

	var got claudia.TaskConfig
	prev := daemonNewTask
	daemonNewTask = func(cfg claudia.TaskConfig) *claudia.Task {
		got = cfg
		return claudia.NewStubTask(cfg, &claudia.StubTaskOps{Run: func(context.Context, claudia.StubTaskRun) (<-chan claudia.TaskEvent, error) {
			ch := make(chan claudia.TaskEvent, 1)
			ch <- claudia.TaskEvent{Type: claudia.TaskEventResult, Content: "pong"}
			close(ch)
			return ch, nil
		}})
	}
	t.Cleanup(func() { daemonNewTask = prev })

	ch, err := claudia.RunBrokerTask(context.Background(), "ping", claudia.TaskConfig{PickByRemaining: true, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if got.Provider != claudia.ProviderCodex {
		t.Fatalf("picked %q, want codex", got.Provider)
	}
	if got.SandboxMode != "workspace-write" || got.ApprovalPolicy != "never" {
		t.Fatalf("codex task sandbox=%q approval=%q, want workspace-write/never", got.SandboxMode, got.ApprovalPolicy)
	}
}

// The spend-limit flag alone does not take a provider with plan room
// out of the pick (2026-10-13: flag true, Claude answering).
func TestTaskPickRemainingKeepsSpendLimitedWithRoom(t *testing.T) {
	f := newFixture(t)
	snap := fleetSnap(t, 15, 40, 99, 30)
	snap[2].Spend = &claudia.PlanSpend{Enabled: true, LimitReached: true}
	opts := f.options(snap)
	opts.DisableResume = true
	opts.resolveBinary = func(claudia.Provider, bool) error { return nil }
	f.bootWith(t, opts)
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })

	var got claudia.Provider
	prev := daemonNewTask
	daemonNewTask = func(cfg claudia.TaskConfig) *claudia.Task {
		got = cfg.Provider
		return claudia.NewStubTask(cfg, &claudia.StubTaskOps{Run: func(context.Context, claudia.StubTaskRun) (<-chan claudia.TaskEvent, error) {
			ch := make(chan claudia.TaskEvent, 1)
			ch <- claudia.TaskEvent{Type: claudia.TaskEventResult, Content: "pong"}
			close(ch)
			return ch, nil
		}})
	}
	t.Cleanup(func() { daemonNewTask = prev })

	ch, err := claudia.RunBrokerTask(context.Background(), "ping", claudia.TaskConfig{PickByRemaining: true, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if got != claudia.ProviderClaude {
		t.Fatalf("picked %q, want claude (fullest; spend flag alone is no veto)", got)
	}
}

// Colossus, 2026-10-13 (pimp-den-casks): a `broker grant --provider codex`
// seat came up read-only. The daemon gives an unrestricted codex grant the
// same workspace-write sandbox, with git write in a repo.
func TestGrantCodexGetsWriteSandbox(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := claudia.Start(claudia.Config{Name: "codex-seat", Provider: claudia.ProviderCodex, WorkDir: repo, SessionID: "sid-c", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start via daemon: %v", err)
	}
	t.Cleanup(a.Stop)
	def := f.d.reg.Def("codex-seat")
	if def == nil {
		t.Fatal("no registered def")
	}
	if def.SandboxMode != "workspace-write" {
		t.Fatalf("codex grant sandbox = %q, want workspace-write", def.SandboxMode)
	}
	if st := f.seat(0).start(); st.Config.SandboxMode != "workspace-write" {
		t.Fatalf("seat started with sandbox %q", st.Config.SandboxMode)
	}
}

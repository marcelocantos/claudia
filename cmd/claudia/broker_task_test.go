// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func TestBrokerUsageAdmitColumn(t *testing.T) {
	ok := 40.0
	sock, _ := startCLIDaemon(t, []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &ok}},
	}})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")

	out := waitUsage(t, func(s string) bool {
		return strings.Contains(s, "ADMIT") && strings.Contains(s, "yes")
	})
	if !strings.Contains(out, "task_run gate") {
		t.Fatalf("usage missing admission legend:\n%s", out)
	}
	if !usageRowAdmits(out, "claude", "yes") {
		t.Fatalf("claude row:\n%s", out)
	}
}

func TestBrokerUsageAdmitNoWhenExhausted(t *testing.T) {
	zero := 0.0
	sock, _ := startCLIDaemon(t, []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &zero}},
	}})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")

	out := waitUsage(t, func(s string) bool {
		return strings.Contains(s, "exhausted")
	})
	if !usageRowAdmits(out, "claude", "no") {
		t.Fatalf("exhausted claude should be ADMIT no:\n%s", out)
	}
}

func TestBrokerUsageJSONListsFleetRoster(t *testing.T) {
	cursor, grok, claude, codex := 12.0, 81.0, 40.0, 0.0
	row := func(p claudia.Provider, rem float64) claudia.PlanUsage {
		pct := rem
		return claudia.PlanUsage{
			Provider: p,
			Status:   claudia.PlanUsageAvailable,
			Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &pct}},
		}
	}
	sock, _ := startCLIDaemon(t, []claudia.PlanUsage{
		row(claudia.ProviderCodex, codex),
		row(claudia.ProviderClaude, claude),
		row(claudia.ProviderGrok, grok),
		row(claudia.ProviderCursor, cursor),
	})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")

	var snap claudia.FleetUsageSnapshot
	backstop := wallclockguard.UntilTestTimeout(t)
	var out string
	for backstop.Err() == nil {
		out = captureStdout(t, func() error { return usageCmd([]string{"-json"}) })
		if json.Unmarshal([]byte(out), &snap) == nil && len(snap.Providers) == 4 &&
			snap.Providers[1].RemainingPercent != nil && *snap.Providers[1].RemainingPercent == 81 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(snap.Providers) != 4 {
		t.Fatalf("usage json:\n%s", out)
	}
	want := []struct {
		p     claudia.Provider
		rem   float64
		admit bool
	}{
		{claudia.ProviderCursor, 12, true},
		{claudia.ProviderGrok, 81, true},
		{claudia.ProviderClaude, 40, true},
		{claudia.ProviderCodex, 0, false},
	}
	for i, w := range want {
		row := snap.Providers[i]
		if row.Provider != w.p || row.RemainingPercent == nil || *row.RemainingPercent != w.rem || row.Admit != w.admit {
			t.Fatalf("providers[%d] = %+v, want %s remaining %v admit %v", i, row, w.p, w.rem, w.admit)
		}
	}
	if snap.FetchedAt.IsZero() {
		t.Fatal("fetched_at is zero")
	}
}

func TestPickRemainingRejectsANamedProvider(t *testing.T) {
	err := taskCmd([]string{"--pick", "remaining", "--provider", "grok", "ping"})
	if err == nil || !strings.Contains(err.Error(), "--pick remaining") {
		t.Fatalf("task err = %v", err)
	}
	err = grantCmd([]string{"--name", "seat", "--pick", "remaining", "--provider", "claude"})
	if err == nil || !strings.Contains(err.Error(), "--pick remaining") {
		t.Fatalf("grant err = %v", err)
	}
	err = grantCmd([]string{"--name", "seat"})
	if err == nil || !strings.Contains(err.Error(), "--provider") {
		t.Fatalf("grant without provider: %v", err)
	}
	err = taskCmd([]string{"--pick", "cheapest", "ping"})
	if err == nil || !strings.Contains(err.Error(), "cheapest") {
		t.Fatalf("bad pick: %v", err)
	}
}

func TestBrokerTaskCLIRunsOverSocket(t *testing.T) {
	ok := 40.0
	sock, _ := startCLIDaemon(t, []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &ok}},
	}})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	marker := filepath.Join(t.TempDir(), "spawned")
	t.Setenv("CLAUDE_BIN", catCLI(t, filepath.Join("..", "..", "testdata", "claude", "exec", "success.jsonl"), marker))

	out := captureStdout(t, func() error {
		return taskCmd([]string{"--provider", "claude", "--workdir", t.TempDir(), "--timeout", "20s", "summarize"})
	})
	if !strings.Contains(out, "Final answer.") {
		t.Fatalf("stdout = %q", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("task_run did not spawn the provider: %v", err)
	}

	jsonOut := captureStdout(t, func() error {
		return taskCmd([]string{"--json", "--provider", "claude", "--workdir", t.TempDir(), "--timeout", "20s", "--prompt", "summarize"})
	})
	for _, want := range []string{`"type":"task_started"`, `"type":"task_event"`, `"type":"task_done"`} {
		if !strings.Contains(jsonOut, want) {
			t.Fatalf("json missing %s:\n%s", want, jsonOut)
		}
	}
}

func TestBrokerTaskCLIRefusesBeforeSpawn(t *testing.T) {
	zero := 0.0
	sock, _ := startCLIDaemon(t, []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows:  []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &zero}},
	}})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	marker := filepath.Join(t.TempDir(), "spawned")
	t.Setenv("CLAUDE_BIN", catCLI(t, filepath.Join("..", "..", "testdata", "claude", "exec", "success.jsonl"), marker))

	err := taskCmd([]string{"--provider", "claude", "--workdir", t.TempDir(), "--timeout", "20s", "summarize"})
	if !errors.Is(err, claudia.ErrPlanExhausted) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("plan_exhausted still spawned the provider")
	}

	out := waitUsage(t, func(s string) bool { return strings.Contains(s, "exhausted") })
	if !usageRowAdmits(out, "claude", "no") {
		t.Fatalf("usage after refusal:\n%s", out)
	}
}

func waitUsage(t *testing.T, ready func(string) bool) string {
	t.Helper()
	backstop := wallclockguard.UntilTestTimeout(t)
	var out string
	for backstop.Err() == nil {
		out = captureStdout(t, func() error { return usageCmd(nil) })
		if ready(out) && !strings.Contains(out, "not fetched yet") {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("usage never ready:\n%s", out)
	return ""
}

// usageRowAdmits reports whether the human usage table's row for provider
// carries the ADMIT word (yes or no).
func usageRowAdmits(out, provider, admit string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == provider && fields[3] == admit {
			return true
		}
	}
	return false
}

// catCLI is a provider binary that records a spawn and prints a fixture.
func catCLI(t *testing.T, fixture, marker string) string {
	t.Helper()
	absFix, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	absMark, err := filepath.Abs(marker)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ntouch '" + absMark + "'\ncat '" + absFix + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

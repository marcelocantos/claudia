// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startGoalAgent(t *testing.T, goal string) (*Agent, *fakeAgentBackend) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	backend := &fakeAgentBackend{name: "fake-goal"}
	agent, err := startWithBackend(Config{
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
		Goal:        goal,
	}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	t.Cleanup(agent.Stop)
	if err := agent.WaitReady(t.Context()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return agent, backend
}

func waitGoalSettle(t *testing.T) {
	t.Helper()
	// Product settle timer is the mechanism under test (🎯T31): elapsing
	// is the PASS. Slack only covers scheduler delay after the timer.
	time.Sleep(waitSettleDuration + 50*time.Millisecond)
}

func backendSends(t *testing.T, b *fakeAgentBackend) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.sends))
	copy(out, b.sends)
	return out
}

func TestParseGoalStatus(t *testing.T) {
	t.Parallel()
	if got, ok := parseGoalStatus("working\n" + GoalStatusComplete + "\n"); !ok || got != GoalStatusComplete {
		t.Fatalf("complete: got %q ok=%v", got, ok)
	}
	if got, ok := parseGoalStatus(GoalStatusBlocked); !ok || got != GoalStatusBlocked {
		t.Fatalf("blocked: got %q ok=%v", got, ok)
	}
	if _, ok := parseGoalStatus("I think I am done"); ok {
		t.Fatal("prose must not count as a status")
	}
	if _, ok := parseGoalStatus("prefix " + GoalStatusComplete); ok {
		t.Fatal("status must be a whole line")
	}
}

func TestGoalContinuationRestatesObjective(t *testing.T) {
	t.Parallel()
	got := goalContinuation("ship T39")
	if !strings.Contains(got, "ship T39") {
		t.Fatalf("continuation dropped the objective:\n%s", got)
	}
	if !strings.Contains(got, GoalStatusComplete) || !strings.Contains(got, GoalStatusBlocked) {
		t.Fatalf("continuation missing status instructions:\n%s", got)
	}
}

func TestGoalIssuesContinuationAfterTerminalTurn(t *testing.T) {
	agent, backend := startGoalAgent(t, "finish the migration")
	if !agent.GoalActive() || agent.Goal() != "finish the migration" {
		t.Fatalf("Goal()=%q active=%v", agent.Goal(), agent.GoalActive())
	}
	if err := agent.Send("start work"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	agent.PublishEvent(Event{Type: "assistant", Text: "looked at files", StopReason: "end_turn"})
	waitGoalSettle(t)
	sends := backendSends(t, backend)
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want 2 (brief + continuation): %#v", len(sends), sends)
	}
	if sends[0] != "start work" {
		t.Fatalf("first send = %q", sends[0])
	}
	if !strings.Contains(sends[1], "finish the migration") {
		t.Fatalf("continuation dropped the objective: %q", sends[1])
	}
	if !agent.GoalActive() {
		t.Fatal("unfinished goal must stay active")
	}
}

func TestEmptyGoalStaysSingleTurn(t *testing.T) {
	agent, backend := startGoalAgent(t, "")
	if agent.GoalActive() {
		t.Fatal("empty Goal must not be active")
	}
	if err := agent.Send("one shot"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	agent.PublishEvent(Event{Type: "assistant", Text: "done", StopReason: "end_turn"})
	waitGoalSettle(t)
	if n := len(backendSends(t, backend)); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

func TestGoalCompleteStatusEndsLoop(t *testing.T) {
	agent, backend := startGoalAgent(t, "all tests pass")
	if err := agent.Send("go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	agent.PublishEvent(Event{
		Type:       "assistant",
		Text:       "tests green\n" + GoalStatusComplete,
		StopReason: "end_turn",
	})
	waitGoalSettle(t)
	if n := len(backendSends(t, backend)); n != 1 {
		t.Fatalf("sends = %d, want 1 after complete", n)
	}
	if agent.GoalActive() {
		t.Fatal("complete must close the goal")
	}
}

func TestGoalBlockedStatusEndsLoop(t *testing.T) {
	agent, backend := startGoalAgent(t, "all tests pass")
	if err := agent.Send("go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	agent.PublishEvent(Event{
		Type:       "assistant",
		Text:       GoalStatusBlocked,
		StopReason: "end_turn",
	})
	waitGoalSettle(t)
	if n := len(backendSends(t, backend)); n != 1 {
		t.Fatalf("sends = %d, want 1 after blocked", n)
	}
	if agent.GoalActive() {
		t.Fatal("blocked must close the goal")
	}
}

func TestGoalInterruptEndsLoop(t *testing.T) {
	agent, backend := startGoalAgent(t, "keep going")
	if err := agent.Send("go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := agent.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	agent.PublishEvent(Event{Type: "assistant", Text: "stopped", StopReason: "end_turn"})
	waitGoalSettle(t)
	if n := len(backendSends(t, backend)); n != 1 {
		t.Fatalf("sends = %d, want 1 after interrupt", n)
	}
	if agent.GoalActive() {
		t.Fatal("interrupt must close the goal")
	}
}

func TestGoalDebouncesMultipleTerminalEvents(t *testing.T) {
	agent, backend := startGoalAgent(t, "one continuation")
	if err := agent.Send("go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	agent.PublishEvent(Event{Type: "assistant", Text: "part a", StopReason: "end_turn"})
	agent.PublishEvent(Event{Type: "assistant", Text: "part b", StopReason: "end_turn"})
	waitGoalSettle(t)
	if n := len(backendSends(t, backend)); n != 2 {
		t.Fatalf("sends = %d, want 2 (one continuation despite two terminals)", n)
	}
}

func TestRegistryLaunchPassesGoal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	cfgs := installFakeRegistryStart(t)
	r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Register(AgentDef{
		Name:      "worker",
		WorkDir:   t.TempDir(),
		SessionID: "sid-goal",
		Goal:      "portable objective",
		Provider:  ProviderGrok,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := r.Launch("worker"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(*cfgs) != 1 {
		t.Fatalf("Launch configs = %d, want 1", len(*cfgs))
	}
	if got := (*cfgs)[0].Goal; got != "portable objective" {
		t.Fatalf("Launch Config.Goal = %q", got)
	}
	if (*cfgs)[0].Provider != ProviderGrok {
		t.Fatalf("Launch Provider = %q", (*cfgs)[0].Provider)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🎯T39 journeys: a sequence of real Agent.Send + backend terminal events
// on an isolated HOME/XDG store. Sequence position is the clock. The
// scripted backend is the regression net; the shipped loop is
// publishEvent → maybeContinueGoal → ops.send.

type scriptedGoalBackend struct {
	fakeAgentBackend
	replies []Event
}

func (b *scriptedGoalBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	start, err := b.fakeAgentBackend.StartAgent(req)
	if err != nil {
		return nil, err
	}
	start.Ops = b.ops()
	return start, nil
}

func (b *scriptedGoalBackend) ops() agentOps {
	base := b.fakeAgentBackend.ops()
	inner := base.send
	base.send = func(a *Agent, msg string) error {
		if err := inner(a, msg); err != nil {
			return err
		}
		b.mu.Lock()
		n := len(b.sends)
		replies := append([]Event(nil), b.replies...)
		b.mu.Unlock()
		if n <= 0 || n > len(replies) {
			return nil
		}
		// Deliver after Send returns so the host timer is the only
		// continuation trigger (same shape as a live turn/completed).
		go a.PublishEvent(replies[n-1])
		return nil
	}
	return base
}

func startJourneyAgent(t *testing.T, cfg Config, backend agentBackend) *Agent {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	cfg.TermLogPath = "-"
	agent, err := startWithBackend(cfg, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	t.Cleanup(agent.Stop)
	if err := agent.WaitReady(t.Context()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return agent
}

func waitBackendSends(t *testing.T, b *fakeAgentBackend, want int) []string {
	t.Helper()
	deadline := time.Now().Add(time.Duration(want)*waitSettleDuration + 200*time.Millisecond)
	for {
		sends := backendSends(t, b)
		if len(sends) >= want {
			return sends
		}
		if time.Now().After(deadline) {
			t.Fatalf("sends = %d, want %d: %#v", len(sends), want, sends)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func unfinishedTurn(text string) Event {
	return Event{Type: "assistant", Text: text, StopReason: "end_turn"}
}

func completeTurn(text string) Event {
	return Event{Type: "assistant", Text: text + "\n" + GoalStatusComplete, StopReason: "end_turn"}
}

// TestGoalJourneyUnfinishedMultiCycle is the load-bearing sequence:
// brief → unfinished → unfinished → complete. Continuation count
// changes across the sequence; a one-shot test cannot see that.
func TestGoalJourneyUnfinishedMultiCycle(t *testing.T) {
	const objective = "land T39 with journeys"
	backend := &scriptedGoalBackend{
		fakeAgentBackend: fakeAgentBackend{name: "journey-seq"},
		replies: []Event{
			unfinishedTurn("inspected files"),
			unfinishedTurn("wrote a test"),
			completeTurn("journeys green"),
		},
	}
	agent := startJourneyAgent(t, Config{Goal: objective}, backend)
	if err := agent.Send("start the work"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	sends := waitBackendSends(t, &backend.fakeAgentBackend, 3)
	if sends[0] != "start the work" {
		t.Fatalf("brief = %q", sends[0])
	}
	if !strings.Contains(sends[1], objective) || !strings.Contains(sends[2], objective) {
		t.Fatalf("a continuation dropped the objective: %#v", sends)
	}
	// One more settle after the complete reply: no fourth Send.
	time.Sleep(waitSettleDuration + 50*time.Millisecond)
	if n := len(backendSends(t, &backend.fakeAgentBackend)); n != 3 {
		t.Fatalf("after complete, sends = %d, want 3", n)
	}
	if agent.GoalActive() {
		t.Fatal("complete must close the goal")
	}
}

// TestGoalJourneyEverySessionBackend proves the loop is on Agent, not
// a Codex-only host: Claude, Grok, and Codex terminal shapes all
// produce one continuation of the same Goal.
func TestGoalJourneyEverySessionBackend(t *testing.T) {
	const objective = "portable across providers"
	codexCompleted, ok, err := parseCodexAppServerLine([]byte(
		`{"method":"turn/completed","params":{"threadId":"thr_j","turn":{"id":"turn_j","status":"completed"}}}`))
	if err != nil || !ok {
		t.Fatalf("codex fixture line: ok=%v err=%v", ok, err)
	}
	codexEv, ok := codexCompleted.agentEvent()
	if !ok {
		t.Fatal("turn/completed must map to an agent Event")
	}
	if !codexEv.IsTerminalStop() {
		t.Fatalf("codex event not terminal: %+v", codexEv)
	}

	cases := []struct {
		name     string
		provider Provider
		terminal Event
	}{
		{name: "claude", provider: ProviderClaude, terminal: unfinishedTurn("claude cycle done")},
		{name: "grok", provider: ProviderGrok, terminal: Event{Type: "assistant", Text: "grok cycle done", StopReason: "end_turn"}},
		{name: "codex", provider: ProviderCodex, terminal: Event{
			Type:       "assistant",
			Text:       "codex cycle done",
			StopReason: codexEv.StopReason,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeAgentBackend{name: "journey-" + tc.name}
			agent := startJourneyAgent(t, Config{Provider: tc.provider, Goal: objective}, backend)
			if err := agent.Send("begin"); err != nil {
				t.Fatalf("Send: %v", err)
			}
			agent.PublishEvent(tc.terminal)
			sends := waitBackendSends(t, backend, 2)
			if sends[0] != "begin" {
				t.Fatalf("brief = %q", sends[0])
			}
			if !strings.Contains(sends[1], objective) {
				t.Fatalf("continuation dropped Goal: %q", sends[1])
			}
			if !agent.GoalActive() {
				t.Fatal("unfinished goal must stay active")
			}
		})
	}
}

// TestGoalJourneyProviderSwitch keeps the same AgentDef.Goal across a
// Grok → Codex remint, the jevons token-balancing move.
func TestGoalJourneyProviderSwitch(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	regPath := filepath.Join(t.TempDir(), "registry.json")
	const objective = "survive the remint"

	r, err := NewRegistry(regPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Register(AgentDef{
		Name:      "worker",
		WorkDir:   t.TempDir(),
		SessionID: "sid-switch",
		Provider:  ProviderGrok,
		Goal:      objective,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	reloaded, err := NewRegistry(regPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	def := reloaded.Def("worker")
	if def == nil || def.Goal != objective || def.Provider != ProviderGrok {
		t.Fatalf("persisted def = %+v", def)
	}

	cfgs := installFakeRegistryStart(t)
	first, err := reloaded.Launch("worker")
	if err != nil {
		t.Fatalf("Launch grok: %v", err)
	}
	if first.Goal() != objective {
		t.Fatalf("first Goal() = %q", first.Goal())
	}
	if (*cfgs)[0].Provider != ProviderGrok || (*cfgs)[0].Goal != objective {
		t.Fatalf("first Launch cfg = %+v", (*cfgs)[0])
	}
	reloaded.Stop("worker")

	def.Provider = ProviderCodex
	if err := reloaded.Register(*def); err != nil {
		t.Fatalf("re-Register as Codex: %v", err)
	}
	second, err := reloaded.Launch("worker")
	if err != nil {
		t.Fatalf("Launch codex: %v", err)
	}
	if second.Goal() != objective || !second.GoalActive() {
		t.Fatalf("second Goal()=%q active=%v", second.Goal(), second.GoalActive())
	}
	if len(*cfgs) != 2 {
		t.Fatalf("Launch configs = %d, want 2", len(*cfgs))
	}
	if (*cfgs)[1].Provider != ProviderCodex || (*cfgs)[1].Goal != objective {
		t.Fatalf("second Launch cfg = %+v", (*cfgs)[1])
	}
}

// TestGoalJourneyCodexTurnCompleted uses the real turn/completed →
// agentEvent mapping, not a hand-built Event, so a parser regression
// cannot leave the loop green.
func TestGoalJourneyCodexTurnCompleted(t *testing.T) {
	const objective = "keep issuing turns"
	backend := &fakeAgentBackend{name: "journey-codex-wire"}
	agent := startJourneyAgent(t, Config{Provider: ProviderCodex, Goal: objective}, backend)
	if err := agent.Send("first cycle"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	raw := `{"method":"turn/completed","params":{"threadId":"thr_j","turn":{"id":"turn_j","status":"completed"}}}`
	parsed, ok, err := parseCodexAppServerLine([]byte(raw))
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	ev, ok := parsed.agentEvent()
	if !ok {
		t.Fatal("turn/completed produced no agent event")
	}
	agent.PublishEvent(ev)
	sends := waitBackendSends(t, backend, 2)
	if sends[0] != "first cycle" {
		t.Fatalf("brief = %q", sends[0])
	}
	if !strings.Contains(sends[1], objective) {
		t.Fatalf("continuation = %q", sends[1])
	}
}

// TestGoalJourneyStopMidSequence: Stop after the first unfinished
// cycle must not emit a later continuation.
func TestGoalJourneyStopMidSequence(t *testing.T) {
	backend := &fakeAgentBackend{name: "journey-stop"}
	agent := startJourneyAgent(t, Config{Goal: "do not resume after stop"}, backend)
	if err := agent.Send("go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	agent.PublishEvent(unfinishedTurn("halfway"))
	_ = waitBackendSends(t, backend, 2)
	agent.Stop()
	time.Sleep(waitSettleDuration + 50*time.Millisecond)
	if n := len(backendSends(t, backend)); n != 2 {
		t.Fatalf("after Stop, sends = %d, want 2", n)
	}
	if agent.GoalActive() {
		t.Fatal("Stop must close the goal")
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Live Goal journeys (🎯T39). Each backend gets one cheap first turn that
// cannot complete the objective, then the host must start a second turn
// without an external nudge. Stop fires as soon as that second turn is
// observed so the run does not grind the Goal to completion.
//
// Gates match the existing live smokes: CLAUDIA_LIVE, CLAUDIA_GROK_LIVE,
// CLAUDIA_CODEX_LIVE.

func TestGoalJourneyLiveBackends(t *testing.T) {
	cases := []struct {
		name string
		gate string
		cfg  Config
		skip func(t *testing.T)
	}{
		{
			name: "claude",
			gate: "CLAUDIA_LIVE",
			cfg: Config{
				Provider:    ProviderClaude,
				Model:       "haiku",
				Goal:        "Produce three numbered observations about this workspace, one per turn.",
				TermLogPath: "-",
			},
			skip: func(t *testing.T) {
				t.Helper()
				if _, err := exec.LookPath("claude"); err != nil {
					t.Skip("claude binary not on PATH")
				}
				if _, err := exec.LookPath("tmux"); err != nil {
					t.Skip("tmux is required for Claude Session")
				}
			},
		},
		{
			name: "grok",
			gate: "CLAUDIA_GROK_LIVE",
			cfg: Config{
				Provider:    ProviderGrok,
				Goal:        "Produce three numbered observations about this workspace, one per turn.",
				TermLogPath: "-",
			},
			skip: func(t *testing.T) {
				t.Helper()
				if _, err := resolveGrokBin(); err != nil {
					t.Skipf("grok binary not found: %v", err)
				}
			},
		},
		{
			name: "codex",
			gate: "CLAUDIA_CODEX_LIVE",
			cfg: Config{
				Provider:    ProviderCodex,
				Goal:        "Produce three numbered observations about this workspace, one per turn.",
				TermLogPath: "-",
			},
			skip: func(t *testing.T) {
				t.Helper()
				if _, err := resolveCodexBin(); err != nil {
					t.Skipf("codex binary not found: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv(tc.gate) == "" {
				t.Skipf("%s not set (this test spends API credit)", tc.gate)
			}
			tc.skip(t)
			runLiveGoalJourney(t, tc.cfg)
		})
	}
}

func runLiveGoalJourney(t *testing.T, cfg Config) {
	t.Helper()
	cfg.WorkDir = t.TempDir()
	agent, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if !agent.GoalActive() {
		t.Fatal("Goal must be active after Start")
	}

	type turnMark struct {
		n      int
		turnID string
		kind   string
	}
	firstDone := make(chan turnMark, 1)
	secondSeen := make(chan turnMark, 1)
	var (
		terminals int
		sawFirst  bool
	)
	tok := agent.SubscribeEvents(func(ev Event) {
		if ev.IsTerminalStop() {
			terminals++
			if terminals == 1 {
				sawFirst = true
				select {
				case firstDone <- turnMark{n: 1, turnID: ev.TurnID, kind: "terminal"}:
				default:
				}
			}
			return
		}
		if !sawFirst {
			return
		}
		if ev.Type == "assistant" || ev.ProgressType == "tool_use" {
			select {
			case secondSeen <- turnMark{n: 2, turnID: ev.TurnID, kind: ev.Type}:
			default:
			}
		}
	})
	defer agent.UnsubscribeEvents(tok)

	if err := agent.WaitReady(t.Context()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	// First user turn is a one-shot that must not complete the Goal.
	if err := agent.Send("Reply with exactly: ping. Do not emit any GOAL_STATUS line."); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var first turnMark
	select {
	case first = <-firstDone:
	case <-ctx.Done():
		t.Fatal("first turn never completed")
	}
	if !agent.GoalActive() {
		t.Fatal("Goal closed after the first turn — host treated ping as a done-report")
	}

	var second turnMark
	select {
	case second = <-secondSeen:
	case <-ctx.Done():
		t.Fatal("no second-turn activity after the first terminal — host did not continue")
	}
	agent.Stop()
	if agent.GoalActive() {
		t.Fatal("Stop must close the Goal")
	}
	t.Logf("live goal journey: first=%+v second=%+v", first, second)
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
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
			name: "cursor",
			gate: "CLAUDIA_CURSOR_LIVE",
			cfg: Config{
				Provider:    ProviderCursor,
				Goal:        "Produce three numbered observations about this workspace, one per turn.",
				TermLogPath: "-",
			},
			skip: func(t *testing.T) {
				t.Helper()
				if _, err := resolveCursorBin(); err != nil {
					t.Skipf("cursor agent binary not found: %v", err)
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
			if err := runLiveGoalJourney(t, tc.cfg, Start, 180*time.Second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func runLiveGoalJourney(t *testing.T, cfg Config, start func(Config) (*Agent, error), timeout time.Duration) error {
	t.Helper()
	cfg.WorkDir = t.TempDir()
	agent, err := start(cfg)
	if err != nil {
		return fmt.Errorf("Start: %w", err)
	}
	defer agent.Stop()
	if !agent.GoalActive() {
		return fmt.Errorf("Goal must be active after Start")
	}

	// secondTurnWatcher decides what a second turn is; see its comment for
	// why counting non-terminal events does not (🎯T77).
	var (
		mu         sync.Mutex
		watcher    secondTurnWatcher
		firstTurn  = make(chan string, 1)
		secondTurn = make(chan turnMark, 1)
		turnError  = make(chan string, 1)
	)
	tok := agent.SubscribeEvents(func(ev Event) {
		if ev.IsError {
			msg := strings.TrimSpace(ev.Text)
			if msg == "" {
				msg = "agent turn failed"
			}
			select {
			case turnError <- msg:
			default:
			}
			return
		}
		mu.Lock()
		defer mu.Unlock()
		hadFirst := watcher.terminals > 0
		second := watcher.Observe(ev)
		if !hadFirst && watcher.terminals > 0 {
			select {
			case firstTurn <- watcher.FirstTurnID():
			default:
			}
		}
		if second {
			select {
			case secondTurn <- watcher.Second:
			default:
			}
		}
	})
	defer agent.UnsubscribeEvents(tok)

	if err := agent.WaitReady(t.Context()); err != nil {
		return fmt.Errorf("WaitReady: %w", err)
	}
	// First user turn is a one-shot that must not complete the Goal.
	if err := agent.Send("Reply with exactly: ping. Do not emit any GOAL_STATUS line."); err != nil {
		return fmt.Errorf("Send: %w", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	var first string
	select {
	case first = <-firstTurn:
	case msg := <-turnError:
		return fmt.Errorf("first turn failed: %s", msg)
	case <-ctx.Done():
		return fmt.Errorf("first turn never completed: %w", ctx.Err())
	}
	if !agent.GoalActive() {
		return fmt.Errorf("Goal closed after the first turn — host treated ping as a done-report")
	}

	var second turnMark
	select {
	case second = <-secondTurn:
	case msg := <-turnError:
		return fmt.Errorf("continuation turn failed: %s", msg)
	case <-ctx.Done():
		return fmt.Errorf("no turn after the first — host did not continue the Goal: %w", ctx.Err())
	}
	mu.Lock()
	rule := watcher.Rule
	mu.Unlock()
	agent.Stop()
	if agent.GoalActive() {
		return fmt.Errorf("Stop must close the Goal")
	}
	t.Logf("live goal journey: first turn %q, second turn %+v decided by %s", first, second, rule)
	return nil
}

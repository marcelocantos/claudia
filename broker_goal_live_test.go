// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// TestBrokerDaemonGoalCompleteCheckLive is 🎯T75.9's live gate: a real
// Claude seat held by an in-process daemon runs its Goal loop on the daemon,
// which asks the consumer's GoalCompleteCheck after each terminal turn. A
// check that says not complete gets a continuation turn; one that says
// complete ends the Goal with no further turn.
func TestBrokerDaemonGoalCompleteCheckLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	for _, bin := range []string{"claude", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	for _, tc := range []struct {
		name     string
		complete bool
	}{
		{"not complete continues", false},
		{"complete ends the goal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startLiveDaemon(t)
			var asked atomic.Int32
			a, err := Start(Config{
				Name: "goal-live-" + newRunID(), Provider: ProviderClaude, Model: "haiku", WorkDir: t.TempDir(),
				Goal: "Produce three numbered observations about this workspace, one per turn.",
				GoalCompleteCheck: func(goal, text string) bool {
					asked.Add(1)
					return tc.complete
				},
			})
			if err != nil {
				t.Fatalf("Start via daemon: %v", err)
			}
			defer a.Stop()
			if a.brokerGrant == "" {
				t.Fatal("seat is not daemon-held")
			}
			var terminals atomic.Int32
			var afterFirst atomic.Int32
			tok := a.SubscribeEvents(func(ev Event) {
				if ev.IsTerminalStop() {
					terminals.Add(1)
					return
				}
				if terminals.Load() >= 1 && (ev.Type == "assistant" || ev.ProgressType == "tool_use") {
					afterFirst.Add(1)
				}
			})
			defer a.UnsubscribeEvents(tok)
			if err := a.WaitReady(t.Context()); err != nil {
				t.Fatalf("WaitReady: %v", err)
			}
			if err := a.Send("Reply with exactly: ping. Do not emit any GOAL_STATUS line."); err != nil {
				t.Fatalf("Send: %v", err)
			}
			wait := func(limit time.Duration, cond func() bool) bool {
				deadline := time.Now().Add(limit)
				for time.Now().Before(deadline) {
					if cond() {
						return true
					}
					time.Sleep(200 * time.Millisecond)
				}
				return false
			}
			if !wait(180*time.Second, func() bool { return terminals.Load() >= 1 }) {
				t.Fatal("first turn never completed")
			}
			if !wait(60*time.Second, func() bool { return asked.Load() >= 1 }) {
				t.Fatal("the daemon never asked the owner's GoalCompleteCheck")
			}
			if tc.complete {
				// Nothing should follow; give a continuation ample time to show.
				time.Sleep(30 * time.Second)
				if n := afterFirst.Load(); n != 0 {
					t.Fatalf("a complete verdict still produced %d event(s) of further work", n)
				}
				if a.GoalActive() {
					t.Fatal("handle still reports the Goal active after a complete verdict")
				}
				return
			}
			if !wait(180*time.Second, func() bool { return afterFirst.Load() >= 1 }) {
				t.Fatal("a not-complete verdict produced no continuation")
			}
			t.Logf("owner asked %d time(s); continuation observed", asked.Load())
		})
	}
}

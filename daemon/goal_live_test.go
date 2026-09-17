// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)

// TestGoalCompleteCheckLive is 🎯T75.9's live gate: a real Claude seat held
// by an in-process daemon runs its Goal loop on the daemon, which asks the
// consumer's GoalCompleteCheck after each terminal turn. A check that says
// not complete gets a continuation turn; one that says complete ends the
// Goal with no further turn.
func TestGoalCompleteCheckLive(t *testing.T) {
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
			a, err := claudia.Start(claudia.Config{
				Name: "goal-live-" + newRunID(), Provider: claudia.ProviderClaude, Model: "haiku", WorkDir: t.TempDir(),
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
			if !a.DaemonHeld() {
				t.Fatal("seat is not daemon-held")
			}
			// afterFirst counts events of any turn after the first. Terminal
			// events alone cannot say that: Claude repeats one terminal
			// message across its content blocks, so the first turn can end
			// more than once. Its turn id, or the continuation prompt showing
			// up in the stream, can.
			var terminals, afterFirst atomic.Int32
			var firstTurn atomic.Value
			tok := a.SubscribeEvents(func(ev claudia.Event) {
				first, _ := firstTurn.Load().(string)
				if terminals.Load() >= 1 && ((ev.TurnID != "" && first != "" && ev.TurnID != first) ||
					strings.Contains(string(ev.Raw), "Continue the open objective")) {
					afterFirst.Add(1)
				}
				if ev.IsTerminalStop() {
					if terminals.Add(1) == 1 {
						firstTurn.Store(ev.TurnID)
					}
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

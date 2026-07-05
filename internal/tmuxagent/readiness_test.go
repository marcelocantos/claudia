// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"strings"
	"testing"
	"time"
)

// Captured-pane fixtures. These stand in for `tmux capture-pane -p`
// output so the detection logic and the auto-advance loop can be
// exercised without a live tmux server or claude binary.

const idleBoxFrame = `● Ready.

────────────────────────────────────────────
❯
────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

// resumeMenuFrame is the T24 wedge: a stale session parks the TUI at a
// resume/summary selection menu awaiting a keypress.
const resumeMenuFrame = `  Do you want to resume this session?

  ❯ 1. Resume from summary
    2. Resume full session
    3. Don't ask again for this session

  Press Enter to confirm · Esc to cancel`

const streamingFrame = `● Rebuilding the maze generator…

  Editing src/maze.go
  ⎿ 42 additions, 3 removals`

func TestMatchReadyDiscriminatesMenu(t *testing.T) {
	tests := []struct {
		name      string
		frame     string
		wantReady bool
		wantMenu  bool
	}{
		{"idle input box", idleBoxFrame, true, false},
		{"resume menu", resumeMenuFrame, false, true},
		{"streaming", streamingFrame, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchReady([]byte(tc.frame)); got != tc.wantReady {
				t.Errorf("MatchReady = %v, want %v", got, tc.wantReady)
			}
			if got := MatchStartupMenu([]byte(tc.frame)); got != tc.wantMenu {
				t.Errorf("MatchStartupMenu = %v, want %v", got, tc.wantMenu)
			}
		})
	}
}

// TestWaitReadyAutoAdvancesResumeMenu is the T24 regression oracle: a
// session that opens on the resume menu must reach ready without
// operator intervention, by the loop pressing Enter for it.
func TestWaitReadyAutoAdvancesResumeMenu(t *testing.T) {
	frames := [][]byte{
		[]byte(resumeMenuFrame), // 1st capture: menu → auto-Enter
		[]byte(resumeMenuFrame), // still repainting → auto-Enter again
		[]byte(idleBoxFrame),    // menu cleared → ready
	}
	call := 0
	enters := 0
	d := readyDriver{
		capture: func() ([]byte, error) {
			f := frames[min(call, len(frames)-1)]
			call++
			return f, nil
		},
		sendEnter: func() error { enters++; return nil },
	}

	elapsed, err := waitReadyLoop(d, time.Millisecond, 2*time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("waitReadyLoop wedged instead of auto-advancing: %v", err)
	}
	if enters == 0 {
		t.Fatal("loop reached ready but never pressed Enter — menu was not auto-confirmed")
	}
	t.Logf("auto-advanced through resume menu in %s with %d Enter(s)", elapsed.Round(time.Millisecond), enters)
}

// TestWaitReadyMenuTimeoutIsDistinct asserts that when Enter never
// clears the menu, the loop gives up after a bounded number of
// confirmations with a distinct, actionable error — not the generic
// "ready pattern did not match" message.
func TestWaitReadyMenuTimeoutIsDistinct(t *testing.T) {
	enters := 0
	d := readyDriver{
		capture:   func() ([]byte, error) { return []byte(resumeMenuFrame), nil },
		sendEnter: func() error { enters++; return nil },
	}

	_, err := waitReadyLoop(d, time.Millisecond, 100*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error when the menu never clears")
	}
	if !strings.Contains(err.Error(), "startup menu") {
		t.Fatalf("error should name the wedged menu, got: %v", err)
	}
	if enters != maxMenuDismissals {
		t.Fatalf("expected exactly %d auto-confirmations, got %d", maxMenuDismissals, enters)
	}
}

// TestWaitReadyReadyImmediately: a normal launch (already at the idle
// box) must not press Enter.
func TestWaitReadyReadyImmediately(t *testing.T) {
	enters := 0
	d := readyDriver{
		capture:   func() ([]byte, error) { return []byte(idleBoxFrame), nil },
		sendEnter: func() error { enters++; return nil },
	}
	if _, err := waitReadyLoop(d, time.Millisecond, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitReadyLoop: %v", err)
	}
	if enters != 0 {
		t.Fatalf("pressed Enter %d time(s) on an already-ready prompt", enters)
	}
}

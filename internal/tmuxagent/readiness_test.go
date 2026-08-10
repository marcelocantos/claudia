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

// resumeMenuFrame is the 🎯T6 wedge: a stale session parks the TUI at a
// resume/summary selection menu awaiting a keypress.
const resumeMenuFrame = `  Do you want to resume this session?

  ❯ 1. Resume from summary
    2. Resume full session
    3. Don't ask again for this session

  Press Enter to confirm · Esc to cancel`

// resumeMenuWordingOnly: selection glyph missing (version drift) but
// Claude's resume copy still present — MatchStartupMenu must fire.
const resumeMenuWordingOnly = `  Do you want to resume this session?

    1. Resume from summary
    2. Resume full session
    3. Don't ask again for this session

  Press Enter to confirm · Esc to cancel`

// numberedMenuCursorOnly: a numbered ❯ selection without resume wording
// (e.g. trust-folder or other startup menus).
const numberedMenuCursorOnly = `  Choose an option:

  ❯ 1. Yes, proceed
    2. No

  Press Enter to confirm`

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
		{"resume wording only", resumeMenuWordingOnly, false, true},
		{"numbered menu cursor only", numberedMenuCursorOnly, false, true},
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

// TestWaitReadyAutoAdvancesResumeMenu is the 🎯T6 regression oracle: a
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

// TestWaitReadyMenuThenSplashThenReady: after menu dismiss, the TUI may
// paint the startup splash before the live composer. WaitReady must
// poll through the splash without more Enter presses into a dead box.
func TestWaitReadyMenuThenSplashThenReady(t *testing.T) {
	frames := []string{resumeMenuFrame, startupSplashFrame, startupSplashFrame, liveComposerFrame}
	i, enters := 0, 0
	d := readyDriver{
		capture: func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return []byte(f), nil
		},
		sendEnter: func() error { enters++; return nil },
	}
	if _, err := waitReadyLoop(d, time.Millisecond, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitReadyLoop: %v", err)
	}
	if enters != 1 {
		t.Fatalf("Enter presses = %d, want 1 (menu only; splash must not be auto-confirmed)", enters)
	}
	if i < 4 {
		t.Errorf("returned after %d capture(s); must poll menu→splash→live", i)
	}
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

// startupSplashFrame is a verbatim `tmux capture-pane -p` frame from a
// real `claude` launch in a fresh workdir (v2.1.224, captured for
// 🎯T284). The composer box is fully drawn and holds the dimmed ghost
// hint, but the TUI is not yet accepting input — keystrokes sent on
// this frame are swallowed.   is the NBSP Claude Code pads the
// prompt glyph with; it is present in the live box too, so it cannot
// itself discriminate.
const startupSplashFrame = "" +
	"\n\n\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"❯ Try \"fix lint errors\"\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents  /rc connecting…\n"

// liveComposerFrame is the same session ~100ms later: the ghost hint is
// gone and the box accepts input.
const liveComposerFrame = "" +
	"\n\n\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"❯ \n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 2 agents                /rc\n"

// typedNotSubmittedFrame: the owner has typed into the live box but has
// not pressed Enter. Still ready — MatchReady deliberately tolerates a
// non-empty composer.
const typedNotSubmittedFrame = "" +
	"\n\n\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"❯ summarise the build failure\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

// TestMatchReadyRejectsStartupSplash is the 🎯T284 oracle: the launch
// splash draws a composer box that satisfies the box pattern, and the
// old signal called it ready ~100ms before the TUI would accept a turn.
// Ready must mean input is accepted.
func TestMatchReadyRejectsStartupSplash(t *testing.T) {
	tests := []struct {
		name       string
		frame      string
		wantReady  bool
		wantSplash bool
	}{
		{"startup splash with ghost hint", startupSplashFrame, false, true},
		{"live empty composer", liveComposerFrame, true, false},
		{"typed but not submitted", typedNotSubmittedFrame, true, false},
		{"resume menu", resumeMenuFrame, false, false},
		{"streaming", streamingFrame, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchReady([]byte(tc.frame)); got != tc.wantReady {
				t.Errorf("MatchReady = %v, want %v", got, tc.wantReady)
			}
			if got := MatchStartupSplash([]byte(tc.frame)); got != tc.wantSplash {
				t.Errorf("MatchStartupSplash = %v, want %v", got, tc.wantSplash)
			}
		})
	}
}

// TestMatchReadyRecognisesMultiLineComposer is the 🎯T25 oracle. Claude
// Code soft-wraps a pasted brief across several rows inside the box; the
// single-line body pattern could not see it, so a pane visibly holding a
// whole unsubmitted brief reported NOT ready and the state had to be
// diagnosed as "unrecognised". All three frames here are verbatim
// captures with a multi-row composer at the tail.
func TestMatchReadyRecognisesMultiLineComposer(t *testing.T) {
	for _, name := range []string{
		// The bug itself: a spawned worker's brief, unsubmitted, no chrome.
		"frame_unsubmitted_brief.txt",
		// A turn is running and the operator has queued a multi-row
		// message; the box is live and accepting more input.
		"frame_queued_during_turn.txt",
		"frame_scrolled_during_turn.txt",
	} {
		t.Run(name, func(t *testing.T) {
			frame := loadFrame(t, name)
			if !MatchReady(frame) {
				t.Errorf("MatchReady = false on a real multi-line composer capture")
			}
			if MatchStartupSplash(frame) {
				t.Errorf("MatchStartupSplash = true on a live composer holding real text")
			}
			if body := composerBody(frame); len(strings.TrimSpace(string(body))) == 0 {
				t.Errorf("composerBody = %q, want the wrapped brief text", body)
			}
		})
	}
}

// transcriptThenIdleComposerFrame is derived (not a verbatim capture):
// an earlier prompt is still echoed in the viewport inside its own rules,
// unindented transcript output follows, and the LIVE EMPTY composer sits
// at the tail. This is the over-broadness guard for the multi-line body
// (🎯T25): the body must stop at the first unindented row, so it reports
// the empty tail composer. Widen composerContinuation to arbitrary rows
// and the body swallows the whole transcript from the earlier ❯ down —
// a dead region of scrollback then reads as a composer holding text.
const transcriptThenIdleComposerFrame = `────────────────────────────────────────────────────────────────────────────────
❯ run the readiness oracles and report the exit status
────────────────────────────────────────────────────────────────────────────────

⏺ Ran the readiness oracles.
  All cases passed.
⏺ Ran go vet.
  No findings.
⏺ Committed the fix.
  Reported the SHA.

────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)                             /rc`

// TestComposerBodyStopsAtUnindentedRow pins the second direction of the
// 🎯T25 acceptance: a looser composer must not make a dead frame look
// live, nor attribute scrollback to the input box.
func TestComposerBodyStopsAtUnindentedRow(t *testing.T) {
	t.Run("live empty composer below an echoed prompt", func(t *testing.T) {
		frame := []byte(transcriptThenIdleComposerFrame)
		if !MatchReady(frame) {
			t.Fatal("MatchReady = false; the empty box at the tail is live")
		}
		if body := composerBody(frame); len(strings.TrimSpace(string(body))) != 0 {
			t.Errorf("composerBody = %q, want empty: the body must stop at the first\n"+
				"unindented row instead of swallowing the transcript above it", body)
		}
		if got := classifyComposer(frame); got != composerEmptyIdle {
			t.Errorf("classifyComposer = %s, want %s", composerStateName(got), composerStateName(composerEmptyIdle))
		}
	})

	// Frames that must stay NOT ready: a drawn-but-dead box and a
	// selection menu are exactly what a looser pattern tends to admit.
	notReady := map[string]string{
		"startup splash (ghost placeholder in a dead box)": startupSplashFrame,
		"/rc still connecting":                             connectingFrame,
		"resume menu":                                      resumeMenuFrame,
		"numbered menu cursor only":                        numberedMenuCursorOnly,
		"streaming output, no box":                         streamingFrame,
	}
	for name, frame := range notReady {
		t.Run(name, func(t *testing.T) {
			if MatchReady([]byte(frame)) {
				t.Errorf("MatchReady = true on a frame that cannot accept a turn")
			}
		})
	}
}

// TestWaitReadyReturnsOnMultiLineComposer: WaitReady must settle on a
// pane whose box holds a wrapped brief, without pressing Enter into it —
// before 🎯T25 that frame was invisible and the loop polled to timeout.
func TestWaitReadyReturnsOnMultiLineComposer(t *testing.T) {
	frames := [][]byte{[]byte(startupSplashFrame), loadFrame(t, "frame_unsubmitted_brief.txt")}
	i, enters := 0, 0
	d := readyDriver{
		capture: func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return f, nil
		},
		sendEnter: func() error { enters++; return nil },
	}
	if _, err := waitReadyLoop(d, time.Millisecond, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitReadyLoop timed out on a pane holding a wrapped brief: %v", err)
	}
	if enters != 0 {
		t.Errorf("pressed Enter %d time(s); a composer is not a menu to auto-confirm", enters)
	}
	if i < 2 {
		t.Errorf("returned ready after %d capture(s); must poll past the splash", i)
	}
}

// TestWaitReadyPollsThroughStartupSplash: the poll loop must not return
// on the splash frame, and must not mistake it for a selection menu and
// start pressing Enter into a dead composer.
func TestWaitReadyPollsThroughStartupSplash(t *testing.T) {
	frames := []string{startupSplashFrame, startupSplashFrame, liveComposerFrame}
	i, enters := 0, 0
	d := readyDriver{
		capture: func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return []byte(f), nil
		},
		sendEnter: func() error { enters++; return nil },
	}
	if _, err := waitReadyLoop(d, time.Millisecond, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitReadyLoop: %v", err)
	}
	if i < 3 {
		t.Errorf("returned ready after %d capture(s); must poll past the splash frames", i)
	}
	if enters != 0 {
		t.Errorf("pressed Enter %d time(s) into the dead splash composer", enters)
	}
}

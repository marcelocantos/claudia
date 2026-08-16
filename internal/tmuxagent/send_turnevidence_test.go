// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"os"
	"strings"
	"testing"
	"time"
)

// virtualClock drives the send loops' timeouts without real sleeping:
// sleep advances the clock the driver reads.
type virtualClock struct{ t time.Time }

func hermeticNow() *virtualClock { return &virtualClock{t: time.Now()} }

func (c *virtualClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func (c *virtualClock) read() time.Time { return c.t }

// 🎯T21 oracles: the turn-began verdict must rest on POSITIVE turn
// evidence. classifyComposer used to conclude "turn began" from the mere
// absence of the idle composer, so SendKeys reported success on frames
// where the brief was still sitting unsubmitted in the pane — the
// fleet-wide "prompt_delivered=true but the agent never started" symptom.
//
// The testdata frames are verbatim `tmux capture-pane -p` output taken
// from the live claudia tmux server on 2026-08-09 while the symptom was
// happening (Claude Code v2.1.226, 80x24 panes):
//
//	frame_unsubmitted_brief.txt   the bug itself — a spawned worker whose
//	                              multi-line brief sits in the composer,
//	                              unsubmitted, with no turn chrome. Since
//	                              🎯T25 readyPattern reads the wrapped body,
//	                              so this is diagnosed as composerTyped
//	                              rather than merely composerUnrecognised.
//	frame_turn_in_progress.txt    a genuinely running turn ("esc to
//	                              interrupt") whose box holds Claude Code's
//	                              queued-messages hint — the composer is
//	                              empty of anything unsubmitted.
//
// frame_turn_in_progress.txt is the over-broadness guard: a fix that
// simply refuses every frame without an empty idle box would break it,
// and with it every legitimately non-idle UI and every fast turn.
//
// 🎯T28 correction: two frames captured in the same batch were labelled
// "queued message during turn" / "composer scrolled during turn" and
// asserted as working — the over-broadness guard for 🎯T21. They are in
// fact the 🎯T28 failure caught on camera a day before it was filed: a
// worker brief sitting in the composer of an agent whose PREVIOUS turn
// is still running. Claude Code vacates the composer the instant a
// message queues and draws "Press up to edit queued messages" in its
// place (frame_turn_in_progress.txt, same batch and same CLI build,
// shows exactly that; so does the 🎯T28 live probe capture
// frame_queued_hint_during_turn.txt), so text in the box is text that
// never queued. They are renamed frame_brief_stuck_during_turn.txt and
// frame_brief_stuck_scrolled.txt and exercised as failures in
// send_t28_midturn_test.go.
func loadFrame(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// midRedrawFrame is derived (not a verbatim capture): the banner is
// painted and the composer's top rule has been drawn, but the ❯ row and
// bottom rule have not. No composer, no chrome — a partial repaint.
const midRedrawFrame = "" +
	"\n ▐▛███▜▌   Claude Code v2.1.226\n" +
	"▝▜█████▛▘  Opus 5 with high effort · Claude Max\n" +
	"  ▘▘ ▝▝    ~/work/github.com/marcelocantos/claudia\n" +
	"\n\n\n" +
	"────────────────────────────────────────────────────────────────────────────────\n"

// postEnterGapFrame is derived (not a verbatim capture): the window
// between Enter and the first spinner paint. Claude Code has cleared the
// composer and echoed the brief, but no turn chrome exists yet. The first
// capture happens at submitSettle (400ms) after Enter, which lands
// squarely here — this is the common case, not a rare race.
const postEnterGapFrame = "" +
	"\n> LOCAL MASTER ONLY (🎯T104): commit locally, no push, no PR, no release.\n" +
	"\n\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)                             /rc\n"

// TestClassifyComposerRequiresPositiveTurnEvidence pins both directions:
// no idle box is never on its own a turn, and real turn chrome still is.
func TestClassifyComposerRequiresPositiveTurnEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame []byte
		want  composerState
	}{
		// Must NOT be working: no positive turn evidence. The brief frame
		// is now named exactly — text present, unsubmitted (🎯T25) —
		// instead of falling through to "unrecognised".
		{"unsubmitted multi-line brief (real capture)", loadFrame(t, "frame_unsubmitted_brief.txt"), composerTyped},
		{"startup/resume menu", []byte(resumeMenuFrame), composerUnrecognised},
		{"mid-redraw, no composer and no chrome", []byte(midRedrawFrame), composerUnrecognised},
		{"post-Enter gap before spinner", []byte(postEnterGapFrame), composerUnrecognised},
		{"startup splash (dead composer)", []byte(startupSplashFrame), composerUnrecognised},
		// Must still count as submitted: real turn evidence with nothing
		// held back in the composer.
		{"turn in progress (real capture)", loadFrame(t, "frame_turn_in_progress.txt"), composerQueued},
		{"queued mid-turn payload (real capture)", loadFrame(t, "frame_queued_hint_during_turn.txt"), composerQueued},
		{"chrome with no composer at the tail", []byte(workingFrame), composerWorking},
		// Unchanged classifications.
		{"idle empty composer", []byte(liveComposerFrame), composerEmptyIdle},
		{"typed but not submitted", []byte(typedNotSubmittedFrame), composerTyped},
		{"collapsed paste chip", []byte(pasteChipFrame), composerPasteChip},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyComposer(tc.frame); got != tc.want {
				t.Errorf("classifyComposer = %s, want %s", composerStateName(got), composerStateName(tc.want))
			}
		})
	}
}

// TestSendKeysRefusesSuccessWithoutTurnEvidence is the core 🎯T21
// regression: on each frame that used to be read as "turn began",
// SendKeys must re-press Enter within the bound and then fail loudly,
// naming the state and quoting the frame tail — never report success.
func TestSendKeysRefusesSuccessWithoutTurnEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		frame     []byte
		wantState composerState
		wantTail  string
	}{
		{"unsubmitted multi-line brief (real capture)", loadFrame(t, "frame_unsubmitted_brief.txt"), composerTyped, "bypass permissions on (shift+tab to cycle)"},
		{"startup/resume menu", []byte(resumeMenuFrame), composerUnrecognised, "Press Enter to confirm"},
		{"mid-redraw, no composer and no chrome", []byte(midRedrawFrame), composerUnrecognised, "────"},
		{"post-Enter gap before spinner", []byte(postEnterGapFrame), composerUnrecognised, "bypass permissions"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, enters := hermeticDriver(
				func() ([]byte, error) { return tc.frame, nil },
				func(string) error { return nil },
				func(string) error { return nil },
			)
			// Short single-line message: typeLiteral, Enter, then confirm.
			err := sendKeysWith(d, "go")
			if err == nil {
				t.Fatalf("SendKeys reported success while the turn had not begun (frame classified %s)",
					composerStateName(classifyComposer(tc.frame)))
			}
			if !strings.Contains(err.Error(), "turn not submitted") {
				t.Errorf("error should name the unsubmitted turn: %v", err)
			}
			if got := classifyComposer(tc.frame); got != tc.wantState {
				t.Errorf("classifyComposer = %s, want %s", composerStateName(got), composerStateName(tc.wantState))
			}
			if !strings.Contains(err.Error(), composerStateName(tc.wantState)) {
				t.Errorf("error should name the %s state: %v", composerStateName(tc.wantState), err)
			}
			if !strings.Contains(err.Error(), tc.wantTail) {
				t.Errorf("error should quote the frame tail (want %q): %v", tc.wantTail, err)
			}
			if *enters < 2 {
				t.Errorf("enters=%d; an unrecognised frame must keep re-pressing Enter within the bound", *enters)
			}
			if *enters > maxSubmitPresses {
				t.Errorf("enters=%d exceeds maxSubmitPresses=%d", *enters, maxSubmitPresses)
			}
		})
	}
}

// TestSendKeysAcceptsRealTurnEvidence is the over-broadness guard: real
// running-turn frames — including ones with no matchable idle composer —
// must still confirm on the first capture, with no extra Enter presses
// hammering a live turn.
func TestSendKeysAcceptsRealTurnEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"frame_turn_in_progress.txt",
		"frame_queued_hint_during_turn.txt",
	} {
		t.Run(name, func(t *testing.T) {
			frame := loadFrame(t, name)
			d, enters := hermeticDriver(
				func() ([]byte, error) { return frame, nil },
				func(string) error { return nil },
				func(string) error { return nil },
			)
			if err := sendKeysWith(d, "go"); err != nil {
				t.Fatalf("SendKeys failed on a frame with real turn evidence: %v", err)
			}
			if *enters != 1 {
				t.Errorf("enters=%d, want 1: a confirmed turn must not be re-pressed", *enters)
			}
		})
	}
}

// TestSendKeysPollsThroughGapUntilTurnEvidence: the gap frame must be
// polled through, not failed on. A fix that errors out on the first
// unrecognised frame would break every normal send.
func TestSendKeysPollsThroughGapUntilTurnEvidence(t *testing.T) {
	t.Parallel()
	working := loadFrame(t, "frame_turn_in_progress.txt")
	frames := [][]byte{[]byte(postEnterGapFrame), []byte(postEnterGapFrame), []byte(postEnterGapFrame), working}
	i := 0
	d, enters := hermeticDriver(
		func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return f, nil
		},
		func(string) error { return nil },
		func(string) error { return nil },
	)
	if err := sendKeysWith(d, "go"); err != nil {
		t.Fatalf("SendKeys must poll through the post-Enter gap, got: %v", err)
	}
	if *enters < 2 {
		t.Errorf("enters=%d; the gap frames should have been re-pressed before chrome appeared", *enters)
	}
}

// TestWaitContentLandedRejectsUnrecognisedFrame: Enter must never fire
// into a pane whose paste has not rendered. Under the old negative test
// the gap frame read as "working", so waitContentLanded returned
// immediately and Enter went out before the paste existed.
func TestWaitContentLandedRejectsUnrecognisedFrame(t *testing.T) {
	t.Parallel()
	frames := [][]byte{
		[]byte(postEnterGapFrame),
		[]byte(midRedrawFrame),
		[]byte(pasteChipFrame),
		loadFrame(t, "frame_turn_in_progress.txt"),
	}
	i := 0
	chipSeen := false
	enters := 0
	d := sendDriver{
		pasteBuffer: func(string) error { return nil },
		typeLiteral: func(string) error { t.Error("multi-line brief must use the paste path"); return nil },
		capture: func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			if classifyComposer(f) == composerPasteChip {
				chipSeen = true
			}
			return f, nil
		},
		sendEnter: func() error {
			enters++
			if !chipSeen {
				t.Error("Enter fired before the pasted brief had rendered in the pane")
			}
			return nil
		},
	}
	// Virtual clock so the poll loops do not sleep in real time.
	now := hermeticNow()
	d.sleep = now.advance
	d.now = now.read

	if err := sendKeysWith(d, "brief line 1\nbrief line 2"); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	if enters == 0 {
		t.Fatal("Enter never pressed")
	}
}

// TestWaitContentLandedTimeoutNamesStateAndTail: when the paste never
// renders, the error must be loud — state plus frame tail, matching the
// 🎯T305 failure-report style.
func TestWaitContentLandedTimeoutNamesStateAndTail(t *testing.T) {
	t.Parallel()
	now := hermeticNow()
	d := sendDriver{
		capture:   func() ([]byte, error) { return []byte(midRedrawFrame), nil },
		sendEnter: func() error { return nil },
		sleep:     now.advance,
		now:       now.read,
	}
	err := waitContentLanded(d, contentLandTimeout)
	if err == nil {
		t.Fatal("want error when the paste never renders")
	}
	if !strings.Contains(err.Error(), composerStateName(composerUnrecognised)) {
		t.Errorf("error should name the unrecognised state: %v", err)
	}
	if !strings.Contains(err.Error(), "last frame tail:") || !strings.Contains(err.Error(), "────") {
		t.Errorf("error should quote the frame tail: %v", err)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"strings"
	"testing"
	"time"
)

// 🎯T121: daemon.TestRewindLive failed with "turn not submitted: composer
// state=typed_unsubmitted after 7 Enter presses" on the post-rewind Send —
// 1 run in 4 from a plain shell at load ~200, and 2 in 3 from inside a
// Claude Code session, so it was not the inherited-marker leak.
//
// A live probe of a `claude --resume` seat at load 130-210 showed why
// (frames: testdata/frame_t121_*.txt, captured 2026-09-22):
//
//	send-keys -l "List every codeword…"   +0.00s  ❯ empty
//	                                       +1.93s  ❯ List every codeword…   (echo)
//	Enter                                  +1.93s  dropped — text stays
//	Enter                                  +2.63s  dropped — text stays
//	Enter                                  +3.58s  dropped — text stays
//	Enter                                  +4.17s  · Noodling… (running UserPromptSubmit hook)
//
// Nothing on screen marks the window in which the TUI drops Enter; it
// simply is not accepting a submit yet, and the window widens with load.
// Across five resumed probes it took 2-4 presses, up to 4.2s after typing.
//
// The typed branch re-pressed on every 400ms sample and gave up when its
// eight-press cap ran out, about three seconds after typing — a clock, and
// the load decided whether the TUI was ready inside it. The cap limits how
// many Enters are sent, which still matters; it was never meant to be the
// span of time a visibly held-back payload is watched. That span is
// submitEvidenceTimeout, and the presses are now spread across it.

// t121PaneDroppingEnterFor is a pane that shows the typed payload and
// drops every Enter pressed before `deaf` has elapsed on the driver's clock,
// then turns the first later Enter into the submitted frame.
func t121PaneDroppingEnterFor(t *testing.T, deaf time.Duration) (sendDriver, *int) {
	t.Helper()
	typed := loadFrame(t, "frame_t121_typed_resumed_enter_dropped.txt")
	submitted := loadFrame(t, "frame_t121_submitted_after_resume.txt")
	var d sendDriver
	var start time.Time
	accepted := false
	d, enters := hermeticDriver(
		func() ([]byte, error) {
			if accepted {
				return submitted, nil
			}
			return typed, nil
		},
		func(string) error { t.Fatal("short single-line message must not paste"); return nil },
		func(string) error { return nil },
	)
	start = d.clock()
	countEnter := d.sendEnter
	d.sendEnter = func() error {
		if d.clock().Sub(start) >= deaf {
			accepted = true
		}
		return countEnter()
	}
	return d, enters
}

func TestT121FramesClassifyAsMeasured(t *testing.T) {
	t.Parallel()
	if got := classifyComposer(loadFrame(t, "frame_t121_typed_resumed_enter_dropped.txt")); got != composerTyped {
		t.Errorf("typed frame classified %s, want typed_unsubmitted", composerStateName(got))
	}
	if got := classifyComposer(loadFrame(t, "frame_t121_submitted_after_resume.txt")); !got.submitted() {
		t.Errorf("submitted frame classified %s, want a submitted state", composerStateName(got))
	}
}

// TestT121ResumedPaneThatDropsEnterForSecondsIsSubmitted is the regression
// oracle. The pane drops Enter for 5s — a little past the 4.2s measured —
// and the send must still land, without pressing more Enters than the cap.
func TestT121ResumedPaneThatDropsEnterForSecondsIsSubmitted(t *testing.T) {
	t.Parallel()
	d, enters := t121PaneDroppingEnterFor(t, 5*time.Second)
	if err := sendKeysWith(d, "List every codeword I have asked you to remember."); err != nil {
		t.Fatalf("a resumed pane that was merely slow to accept Enter was refused: %v", err)
	}
	if *enters > maxSubmitPresses {
		t.Errorf("enters=%d exceeds maxSubmitPresses=%d: spreading the presses must not add any",
			*enters, maxSubmitPresses)
	}
}

// TestT121PaneThatNeverAcceptsEnterStillFailsLoudly is the over-broadness
// guard: a payload that sits in the composer for the whole evidence bound
// is still refused, still named typed_unsubmitted, and never costs more
// than the cap's worth of Enters.
func TestT121PaneThatNeverAcceptsEnterStillFailsLoudly(t *testing.T) {
	t.Parallel()
	d, enters := t121PaneDroppingEnterFor(t, 24*time.Hour)
	start := d.clock()
	err := sendKeysWith(d, "List every codeword I have asked you to remember.")
	if err == nil {
		t.Fatal("want an error when the typed payload is never submitted")
	}
	if !strings.Contains(err.Error(), "typed_unsubmitted") {
		t.Errorf("error should name the held-back state: %v", err)
	}
	if *enters > maxSubmitPresses {
		t.Errorf("enters=%d exceeds maxSubmitPresses=%d", *enters, maxSubmitPresses)
	}
	if elapsed := d.clock().Sub(start); elapsed > submitEvidenceTimeout+10*time.Second {
		t.Errorf("watched for %s, want no more than about submitEvidenceTimeout (%s)", elapsed, submitEvidenceTimeout)
	}
}

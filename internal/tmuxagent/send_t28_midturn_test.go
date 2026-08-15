// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"strings"
	"testing"
)

// 🎯T28 oracles: a payload delivered while the agent is ALREADY running a
// turn is either submitted or reported as a failure — never silently
// left in the composer.
//
// The lost submit was here, in this package. classifyComposer tested
// turn chrome first, so on a mid-turn send the chrome of the RUNNING
// turn satisfied both gates before either could look at the composer:
// waitContentLanded returned on the first capture (Enter fired before
// the paste had rendered) and ensureSubmitted returned nil after zero
// Enter presses. Nothing about the payload was ever checked, which is
// why the caller was told "sent" while the brief sat in the box.
//
// Fixtures, all verbatim `tmux capture-pane -p` output:
//
//	frame_t28_probe.txt              the live specimen, captured 2026-08-10
//	                                 by claudia-po while spawning the worker
//	                                 for this target: "✽ Julienning… ↓ 2.7k
//	                                 tokens" running, and the tail of an
//	                                 unsubmitted brief in the box below it.
//	frame_brief_stuck_during_turn.txt the same failure captured 2026-08-09
//	frame_brief_stuck_scrolled.txt    (see the 🎯T28 correction note in
//	                                 send_turnevidence_test.go — these two
//	                                 were mislabelled "queued" and asserted
//	                                 as working, which is what made the
//	                                 chrome-first rule look safe).
//	frame_queued_hint_during_turn.txt the contrast, from the 🎯T28 live
//	                                 probe: the same mid-turn send when it
//	                                 DOES queue. The payload is echoed above
//	                                 the box, the box holds "Press up to
//	                                 edit queued messages", and the footer
//	                                 still says "paste again to expand".
var t28StuckFrames = []string{
	"frame_t28_probe.txt",
	"frame_brief_stuck_during_turn.txt",
	"frame_brief_stuck_scrolled.txt",
}

// TestT28ChromeOfAnotherTurnIsNotSubmitEvidence pins the classification:
// text in the composer is unsubmitted text even when a turn is visibly
// running, because that turn is not necessarily ours.
func TestT28ChromeOfAnotherTurnIsNotSubmitEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range t28StuckFrames {
		t.Run(name, func(t *testing.T) {
			frame := loadFrame(t, name)
			if !MatchTurnInProgress(frame) {
				t.Fatalf("fixture is not a mid-turn capture; the 🎯T28 case needs turn chrome on the pane")
			}
			if got := classifyComposer(frame); got != composerTyped {
				t.Errorf("classifyComposer = %s, want %s: the composer is holding an unsubmitted brief",
					composerStateName(got), composerStateName(composerTyped))
			}
		})
	}
}

// TestT28SendReportsFailureWhenPayloadStaysInComposer is the core
// contract (🎯T28 acceptance 4): Send must return an error rather than
// nil when it cannot confirm the payload left the composer.
func TestT28SendReportsFailureWhenPayloadStaysInComposer(t *testing.T) {
	t.Parallel()
	for _, name := range t28StuckFrames {
		t.Run(name, func(t *testing.T) {
			frame := loadFrame(t, name)
			d, enters := hermeticDriver(
				func() ([]byte, error) { return frame, nil },
				func(string) error { return nil },
				func(string) error { return nil },
			)
			err := sendKeysWith(d, "mid-turn brief line 1\nmid-turn brief line 2\n")
			if err == nil {
				t.Fatalf("Send reported success while the payload sat in the composer (frame classified %s)",
					composerStateName(classifyComposer(frame)))
			}
			if !strings.Contains(err.Error(), "turn not submitted") {
				t.Errorf("error should name the unsubmitted turn: %v", err)
			}
			if !strings.Contains(err.Error(), composerStateName(composerTyped)) {
				t.Errorf("error should name the %s state: %v", composerStateName(composerTyped), err)
			}
			if !strings.Contains(err.Error(), "last frame tail:") {
				t.Errorf("error should quote the frame tail: %v", err)
			}
			if *enters < 2 {
				t.Errorf("enters=%d; a stuck composer must be re-pressed within the bound", *enters)
			}
			if *enters > maxSubmitPresses {
				t.Errorf("enters=%d exceeds maxSubmitPresses=%d", *enters, maxSubmitPresses)
			}
		})
	}
}

// TestT28QueuedPayloadReportsSuccess is the over-broadness guard in the
// same plane: when the mid-turn payload really does queue, Send must
// report success and stop pressing. Before 🎯T28 this frame failed —
// the stale "paste again to expand" footer read as a stuck paste chip,
// so a payload that had landed was reported as not submitted after
// eight further Enter presses into a live session.
func TestT28QueuedPayloadReportsSuccess(t *testing.T) {
	t.Parallel()
	frame := loadFrame(t, "frame_queued_hint_during_turn.txt")
	if !pasteExpandHint.Match(frame) {
		t.Fatal("fixture no longer carries the stale expand hint that made this over-broad")
	}
	if got := classifyComposer(frame); got != composerQueued {
		t.Fatalf("classifyComposer = %s, want %s", composerStateName(got), composerStateName(composerQueued))
	}
	if MatchUnsubmittedPaste(frame) {
		t.Error("a queued payload must not read as an unsubmitted paste chip")
	}
	d, enters := hermeticDriver(
		func() ([]byte, error) { return frame, nil },
		func(string) error { return nil },
		func(string) error { return nil },
	)
	// landed=true: the payload rendered in the box before Enter, which is
	// waitContentLanded's postcondition on the paste branch (🎯T30).
	if err := ensureSubmitted(d, true); err != nil {
		t.Fatalf("Send failed on a payload the CLI itself reports as queued: %v", err)
	}
	if *enters != 0 {
		t.Errorf("enters=%d, want 0: a queued payload must not be hammered with more Enters", *enters)
	}
}

// TestT28WaitContentLandedIgnoresAnotherTurnsChrome pins the first of
// the two short-circuits: Enter must not fire until the payload is
// visible in the pane, even though a turn is already running.
func TestT28WaitContentLandedIgnoresAnotherTurnsChrome(t *testing.T) {
	t.Parallel()
	midTurn := loadFrame(t, "frame_turn_in_progress.txt")
	if !MatchTurnInProgress(midTurn) {
		t.Fatal("fixture must show a running turn")
	}
	now := hermeticNow()
	captures := 0
	d := sendDriver{
		capture:   func() ([]byte, error) { captures++; return midTurn, nil },
		sendEnter: func() error { t.Error("Enter fired before the paste had rendered"); return nil },
		sleep:     now.advance,
		now:       now.read,
	}
	err := waitContentLanded(d, contentLandTimeout)
	if err == nil {
		t.Fatal("waitContentLanded returned on another turn's chrome; the paste never rendered")
	}
	if !strings.Contains(err.Error(), "paste never appeared") {
		t.Errorf("error should name the missing paste: %v", err)
	}
	if captures < 2 {
		t.Errorf("captures=%d; the loop must keep polling for the payload", captures)
	}
}

// TestT28MidTurnPayloadThatSubmitsOnRetry is the end-to-end shape of a
// healthy mid-turn send: chrome on the pane throughout, the paste
// renders, one Enter is absorbed while the pane repaints, the second
// queues it. Send must confirm from the queued hint, not from the
// chrome that was there all along.
func TestT28MidTurnPayloadThatSubmitsOnRetry(t *testing.T) {
	t.Parallel()
	stuck := loadFrame(t, "frame_t28_probe.txt")
	queued := loadFrame(t, "frame_queued_hint_during_turn.txt")
	frames := [][]byte{stuck, stuck, stuck, queued}
	i := 0
	pasted := ""
	enters := 0
	now := hermeticNow()
	d := sendDriver{
		pasteBuffer: func(msg string) error { pasted = msg; return nil },
		typeLiteral: func(string) error { t.Error("multi-line brief must use the paste path"); return nil },
		capture: func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return f, nil
		},
		sendEnter: func() error {
			enters++
			if pasted == "" {
				t.Error("Enter fired before the payload was pasted")
			}
			return nil
		},
		sleep: now.advance,
		now:   now.read,
	}
	msg := "mid-turn brief line 1\nmid-turn brief line 2\n" + strings.Repeat("x", pasteBlockThreshold)
	if err := sendKeysWith(d, msg); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	if pasted != msg {
		t.Errorf("pasteBuffer got %q, want the full message", pasted)
	}
	if enters < 2 {
		t.Errorf("enters=%d, want >=2 (initial Enter plus the retry that queued it)", enters)
	}
}

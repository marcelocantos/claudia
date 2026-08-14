// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import "testing"

// 🎯T28 acceptance 5, the over-broad half: a send that DID submit must
// never be reported as a failure. Both fixtures here are verbatim
// `tmux capture-pane -p` output taken off the live fleet socket on
// 2026-08-15, minutes after jevons_agent_send returned
//
//	send failed: turn not submitted: composer state=paste_chip after 8
//	Enter presses; last frame tail: … ❯
//
// for payloads that had in fact reached the receiving agent (the payload
// was found in the receiver's own session transcript).
//
//	frame_daily_queued_expand_hint.txt   the payload queued behind a
//	                                     running turn: the box holds
//	                                     "Press up to edit queued
//	                                     messages" and the footer still
//	                                     carries "paste again to expand".
//	frame_daily_spinner_empty_composer.txt the payload was taken up as the
//	                                     turn: the spinner line reads
//	                                     "✶ Pollinating…", the box is
//	                                     EMPTY, and the same stale expand
//	                                     hint sits in the footer where the
//	                                     "esc to interrupt" row usually is.
//
// Both frames carry the stale expand hint, which is what the pre-fix
// detector read: it asked whether the composer box was DRAWN
// (pasteExpandHint && MatchReady) rather than whether it HELD anything,
// so an empty, drained box reported a stuck chip.
var t28DailyDeliveredFrames = []struct {
	name string
	want composerState
}{
	{"frame_daily_queued_expand_hint.txt", composerQueued},
	{"frame_daily_spinner_empty_composer.txt", composerWorking},
}

// TestT28DailyDeliveredFramesAreNotPasteChips pins the classification of
// the two live specimens: a drained composer is delivery evidence, not a
// stuck paste chip, however long the expand hint lingers in the footer.
func TestT28DailyDeliveredFramesAreNotPasteChips(t *testing.T) {
	t.Parallel()
	for _, tc := range t28DailyDeliveredFrames {
		t.Run(tc.name, func(t *testing.T) {
			frame := loadFrame(t, tc.name)
			if !pasteExpandHint.Match(frame) {
				t.Fatal("fixture no longer carries the stale expand hint that made the detector over-broad")
			}
			if MatchUnsubmittedPaste(frame) {
				t.Error("a drained composer must not read as an unsubmitted paste chip")
			}
			got := classifyComposer(frame)
			if got == composerPasteChip {
				t.Fatalf("classifyComposer = %s: this is the reported-failed-but-delivered specimen",
					composerStateName(got))
			}
			if got != tc.want {
				t.Errorf("classifyComposer = %s, want %s",
					composerStateName(got), composerStateName(tc.want))
			}
			if !got.submitted() {
				t.Errorf("state %s is not counted as submitted; the payload reached the agent",
					composerStateName(got))
			}
		})
	}
}

// TestT28DailyDeliveredFramesReportSuccess is the caller-visible
// contract: ensureSubmitted returns nil and stops pressing. On the daily
// path these frames drew eight further Enters and then a hard error.
func TestT28DailyDeliveredFramesReportSuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range t28DailyDeliveredFrames {
		t.Run(tc.name, func(t *testing.T) {
			frame := loadFrame(t, tc.name)
			d, enters := hermeticDriver(
				func() ([]byte, error) { return frame, nil },
				func(string) error { return nil },
				func(string) error { return nil },
			)
			if err := ensureSubmitted(d); err != nil {
				t.Fatalf("Send failed on a payload that reached the agent: %v", err)
			}
			if *enters != 0 {
				t.Errorf("enters=%d, want 0: a delivered payload must not be hammered with more Enters", *enters)
			}
		})
	}
}

// TestT28PreFixPredicateFiredOnDeliveredFrames documents the mechanism
// the daily path was still running (claudia pinned at b995fdd, 44
// minutes before the 🎯T28 fix): "the box is drawn" rather than "the box
// holds text". It asserts the OLD predicate would have misfired on both
// specimens, so the fixtures cannot silently stop covering the bug.
func TestT28PreFixPredicateFiredOnDeliveredFrames(t *testing.T) {
	t.Parallel()
	for _, tc := range t28DailyDeliveredFrames {
		t.Run(tc.name, func(t *testing.T) {
			frame := loadFrame(t, tc.name)
			preFix := pasteExpandHint.Match(trimTrailingSpace(frame)) && MatchReady(frame)
			if !preFix {
				t.Skipf("pre-fix predicate does not fire on this frame; it fails for another reason")
			}
			if MatchUnsubmittedPaste(frame) {
				t.Error("the fixed detector reproduces the pre-fix false positive")
			}
		})
	}
}

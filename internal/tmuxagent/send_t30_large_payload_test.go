// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"strings"
	"testing"
)

// 🎯T30 oracles: a payload big enough to collapse into a Claude Code
// paste chip is SUBMITTED, not merely refused honestly. 🎯T28 closed the
// silent-loss half (submitted or reported as failure); this is the other
// half, where every finish-report-sized send was reported as a failure
// and could not be delivered at all.
//
// THE LIVE REPRODUCTION, captured before any fix by cmd/t30send driving
// the shipped path (Agent.Send → SendKeys → waitContentLanded →
// ensureSubmitted) against a real agent on 2026-08-15, 08:46:16–19,
// Claude Code v2.1.226, 80x24 pane. A 6400-byte payload into an IDLE
// composer:
//
//	send returned after 3.13s: ERROR turn not submitted: composer empty
//	after paste (brief never reached pane)
//	verdict: send=refused delivered=true
//
// delivered=true is the whole target in one line: the model had the
// payload and answered it while SendKeys told the caller the brief never
// reached the pane. The 300-byte control send in the same run — below
// pasteBlockThreshold, so typed rather than pasted — returned nil.
//
// The two frames of that send, verbatim `tmux capture-pane -p`:
//
//	frame_t30_chip_before_enter.txt          08:46:18.241 — the collapsed
//	                                         chip in the box, "[Pasted
//	                                         text #1 +1 lines]", with
//	                                         "paste again to expand" in
//	                                         the footer row. This is what
//	                                         waitContentLanded returned on.
//	frame_t30_spinner_drained_after_enter.txt 08:46:18.729 — 488ms later:
//	                                         the payload is echoed above
//	                                         the box as the submitted
//	                                         turn, the activity line reads
//	                                         "· Unravelling…", and the box
//	                                         is EMPTY. The turn had begun.
//
// MECHANISM (send.go). ensureSubmitted classified that second frame as
// composerEmptyIdle and, with sawContent false, returned "composer empty
// after paste (brief never reached pane)". Two independent classifier
// defects put it there, and both are size-selective in exactly the way
// 🎯T30 describes, because only a payload at or above pasteBlockThreshold
// (400 bytes) takes the bracketed-paste branch that produces this UI at
// all:
//
//   - MatchTurnInProgress did not know the activity line. Its hint list
//     names spinner glyphs and "esc to interrupt", and on a pasted turn
//     the footer that would carry "esc to interrupt" is displaced by the
//     lingering "paste again to expand" hint, so a running turn showed no
//     recognised chrome. spinnerLine (send.go:46) closed this, landed
//     alongside 🎯T28's stale-expand-hint fix.
//   - ensureSubmitted started blind: waitContentLanded had already
//     watched the payload render in the box, and threw that fact away.
//     A box that empties under our own Enter after the payload was seen
//     in it is submit evidence, not absence evidence. The landed
//     parameter (send.go:359) carries it in.
//
// The second is the belt to the first's braces, and it is what the 🎯T21
// implementer declared as a residual and declined to weaken then:
// "ensureSubmitted still errors when content is seen then the composer
// empties with no chrome; a turn completing before the first 400ms
// capture can land there". 🎯T30 is that residual arriving as a product
// failure, so it is now closed rather than declared.
//
// MUTATION EVIDENCE, quoted (send.go restored byte-identical after each,
// sha1 194ccb7f72cfeb3954751a75495ce503a477a5fb; the over-broad direction
// is last):
//
//	M1a  sawContent := landed  ->  sawContent := false  (start blind again)
//	     KILLED by TestT30VacatedComposerIsSubmitEvidence.
//	M1b  the vacated-box `return nil` restored to the old
//	     re-press-within-the-bound block
//	     KILLED by TestT30VacatedComposerIsSubmitEvidence.
//	M1a + M3 together — the whole pre-fix pipeline, both defects back —
//	     KILLED by TestT30LiveReproSendSucceeds, and it fails with the
//	     live error verbatim: "turn not submitted: composer empty after
//	     paste (brief never reached pane)". Each defect alone is enough to
//	     make the captured repro pass, which is why both are named.
//	M3   MatchTurnInProgress reverted to turnInProgressHint only (🎯T28's
//	     half) KILLED by TestT28DailyDeliveredFramesAreNotPasteChips/
//	     frame_daily_spinner_empty_composer.txt.
//	M2   OVER-BROADNESS: landed := false -> landed := true in
//	     sendKeysWith, i.e. trust every send as if its payload had been
//	     watched landing. KILLED by
//	     TestT30EmptyComposerWithoutLandedEvidenceStillFails. The fix
//	     cannot silently become "an empty box always means submitted",
//	     and TestT30SubThresholdSendUnaffected pins that the small sends
//	     that worked before still take the typed branch and still confirm
//	     from turn chrome.

// t30LargePayload is a payload of the size that provoked the target: a
// 5511-character finish report failed on the live path, and 🎯T30
// clause 3 sets the floor at 6000. It goes down the bracketed-paste
// branch by size alone, with no newline to help it.
func t30LargePayload() string {
	const size = 6400
	body := "T30 finish report. " + strings.Repeat("This paragraph pads the payload to the size a real finish report reaches. ", 100)
	return body[:size]
}

// TestT30LargePayloadUsesPasteBranch pins the precondition the rest of
// this file rests on: at 🎯T30's size the send is a bracketed paste, so
// it is the paste branch — not the typed one — that must submit.
func TestT30LargePayloadUsesPasteBranch(t *testing.T) {
	t.Parallel()
	msg := t30LargePayload()
	if len(msg) < 6000 {
		t.Fatalf("payload is %d bytes; 🎯T30 clause 3 requires at least 6000", len(msg))
	}
	if strings.Contains(msg, "\n") {
		t.Fatal("payload must select the paste branch by SIZE alone, with no newline")
	}
	if !useBracketedPaste(msg) {
		t.Fatalf("a %d-byte payload must take the bracketed-paste branch (threshold %d)",
			len(msg), pasteBlockThreshold)
	}
}

// TestT30LiveReproSendSucceeds replays the captured repro through the
// whole send path. This is the failure itself, frame for frame: the chip
// waitContentLanded returned on, then the drained box the submit loop
// read as "brief never reached pane". SendKeys must now report the
// success the transcript already proved.
func TestT30LiveReproSendSucceeds(t *testing.T) {
	t.Parallel()
	chip := loadFrame(t, "frame_t30_chip_before_enter.txt")
	drained := loadFrame(t, "frame_t30_spinner_drained_after_enter.txt")
	if classifyComposer(chip) != composerPasteChip {
		t.Fatalf("fixture 1 is %s, want the collapsed chip", composerStateName(classifyComposer(chip)))
	}
	if composerHoldsUnsubmitted(drained) {
		t.Fatal("fixture 2 must be the DRAINED box; the payload had already left it")
	}

	msg := t30LargePayload()
	pasted := ""
	enters := 0
	i := 0
	now := hermeticNow()
	d := sendDriver{
		pasteBuffer: func(m string) error { pasted = m; return nil },
		typeLiteral: func(string) error { t.Error("a 6400-byte payload must not be typed"); return nil },
		capture: func() ([]byte, error) {
			// Capture 1 answers the /rc-connecting guard, capture 2 is
			// what waitContentLanded returned on, and everything after
			// is the pane the submit loop actually decided from.
			i++
			if i <= 2 {
				return chip, nil
			}
			return drained, nil
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
	if err := sendKeysWith(d, msg); err != nil {
		t.Fatalf("sendKeysWith on the live 🎯T30 repro: %v", err)
	}
	if pasted != msg {
		t.Errorf("pasteBuffer got %d bytes, want the whole %d-byte payload", len(pasted), len(msg))
	}
	if enters != 1 {
		t.Errorf("enters=%d, want 1: the payload was gone from the box after the first Enter", enters)
	}
}

// TestT30VacatedComposerIsSubmitEvidence isolates the second defect from
// the first. The fixture is DERIVED, not a capture: it is
// frame_t30_spinner_drained_after_enter.txt with the activity line
// removed, so the pane carries no turn chrome of any kind — the payload
// echoed above a box that is now empty. Sampling that window live needs
// a capture inside a gap this CLI closed in under 135ms (cmd/t30send at
// -interval 40ms never caught one), which is precisely why the frame is
// derived and labelled so.
//
// It is the state 🎯T21 declared as a residual false-failure class, and
// under the old blind loop it returns "brief never reached pane" for a
// payload the box was watched taking up. Reverting `sawContent := landed`
// to `sawContent := false` in ensureSubmitted turns this test RED.
func TestT30VacatedComposerIsSubmitEvidence(t *testing.T) {
	t.Parallel()
	vacated := loadFrame(t, "frame_t30_vacated_no_chrome_derived.txt")
	if MatchTurnInProgress(vacated) {
		t.Fatal("fixture must carry NO turn chrome; otherwise it tests spinnerLine, not the landed evidence")
	}
	if got := classifyComposer(vacated); got != composerEmptyIdle {
		t.Fatalf("classifyComposer = %s, want %s", composerStateName(got), composerStateName(composerEmptyIdle))
	}

	enters := 0
	now := hermeticNow()
	d := sendDriver{
		pasteBuffer: func(string) error { return nil },
		capture:     func() ([]byte, error) { return vacated, nil },
		sendEnter:   func() error { enters++; return nil },
		sleep:       now.advance,
		now:         now.read,
	}
	// landed=true is waitContentLanded's postcondition on the paste
	// branch: the payload was seen in this box before Enter.
	if err := ensureSubmitted(d, true); err != nil {
		t.Fatalf("a box the payload was seen in, emptied by our own Enter, is submit evidence: %v", err)
	}
	if enters != 0 {
		t.Errorf("enters=%d, want 0: hammering Enter into a submitted turn is what burned the bound", enters)
	}
}

// TestT30EmptyComposerWithoutLandedEvidenceStillFails is the
// over-broadness guard for the same change, and the reason it is a
// parameter rather than an unconditional relaxation. When the payload
// was never seen in the box — the typed branch, or a paste that never
// rendered — an empty composer is still absence evidence, and Send must
// still refuse loudly (🎯T305 / 🎯T21). Mutating `landed := false` to
// `landed := true` in sendKeysWith, or `sawContent := landed` to
// `sawContent := true`, turns this test RED.
func TestT30EmptyComposerWithoutLandedEvidenceStillFails(t *testing.T) {
	t.Parallel()
	vacated := loadFrame(t, "frame_t30_vacated_no_chrome_derived.txt")

	msg := "a short brief that never made it into the box"
	if useBracketedPaste(msg) {
		t.Fatalf("this case is the typed branch, where nothing is ever watched landing (%d bytes)", len(msg))
	}
	now := hermeticNow()
	d := sendDriver{
		typeLiteral: func(string) error { return nil },
		pasteBuffer: func(string) error { t.Error("sub-threshold payload must not be pasted"); return nil },
		capture:     func() ([]byte, error) { return vacated, nil },
		sendEnter:   func() error { return nil },
		sleep:       now.advance,
		now:         now.read,
	}
	err := sendKeysWith(d, msg)
	if err == nil {
		t.Fatal("an empty composer for a payload never seen landing must not be reported as submitted")
	}
	if !strings.Contains(err.Error(), "brief never reached pane") {
		t.Errorf("error should name the missing brief: %v", err)
	}
}

// TestT30SubThresholdSendUnaffected is the other half of the
// over-broadness evidence: the small sends that worked before 🎯T30 must
// still work, by the same route they always did. Both frames are
// verbatim captures from the 300-byte control send of the same live run
// that produced the repro (08:46:23–24) — typed into the box, then the
// turn running with the box empty.
func TestT30SubThresholdSendUnaffected(t *testing.T) {
	t.Parallel()
	typed := loadFrame(t, "frame_t30_small_typed_before_enter.txt")
	working := loadFrame(t, "frame_t30_small_working_after_enter.txt")
	if got := classifyComposer(typed); got != composerTyped {
		t.Fatalf("fixture 1 is %s, want %s", composerStateName(got), composerStateName(composerTyped))
	}
	if got := classifyComposer(working); got != composerWorking {
		t.Fatalf("fixture 2 is %s, want %s", composerStateName(got), composerStateName(composerWorking))
	}

	msg := "T30 control send, below the paste threshold."
	if useBracketedPaste(msg) {
		t.Fatalf("control payload must stay on the typed branch (%d bytes, threshold %d)",
			len(msg), pasteBlockThreshold)
	}
	typedLiteral := ""
	enters := 0
	i := 0
	now := hermeticNow()
	d := sendDriver{
		typeLiteral: func(m string) error { typedLiteral = m; return nil },
		pasteBuffer: func(string) error { t.Error("a sub-threshold payload must not be pasted"); return nil },
		capture: func() ([]byte, error) {
			// Capture 1 answers the /rc-connecting guard (any
			// non-connecting frame does); the rest is the post-Enter pane.
			i++
			if i == 1 {
				return typed, nil
			}
			return working, nil
		},
		sendEnter: func() error { enters++; return nil },
		sleep:     now.advance,
		now:       now.read,
	}
	if err := sendKeysWith(d, msg); err != nil {
		t.Fatalf("sendKeysWith on the sub-threshold control: %v", err)
	}
	if typedLiteral != msg {
		t.Errorf("typeLiteral got %q, want the message", typedLiteral)
	}
	if enters != 1 {
		t.Errorf("enters=%d, want 1", enters)
	}
}

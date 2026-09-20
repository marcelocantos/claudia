// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"strings"
	"testing"
	"time"
)

// 🎯T101: the Claude live gate could not be run on a loaded host. Every
// attempt failed at Send with "composer empty after paste (brief never
// reached pane)" for a 16-byte prompt that takes the TYPED branch, and a
// control at unmodified HEAD on a pristine worktree reproduced it — so
// it was a repo fact, not one seat's diff.
//
// What the pane was actually doing, measured frame by frame on the live
// path at load average ~200 (probe frames: testdata/frame_t101_*.txt):
//
//	WaitReady                     16.58s   composer finally drawn
//	send-keys -l "respond…"       +0.00s   ❯ still EMPTY
//	  … +2.00s   ❯ still EMPTY — nothing echoed at all
//	Enter                         +0.00s   ❯ still EMPTY
//	  … +0.20s   ❯ still EMPTY
//	  … +0.40s   ❯ respond with: ok   (echo, at last)
//	  … +1.60s   ✢ Ruminating…        (turn chrome)
//
// The old submit loop gave that pane exactly two samples — 800ms — and
// on the second empty frame declared the brief lost. At load 243-335 the
// echo lands after that, so the send was refused for a turn the model
// then ran: a false refusal, the same defect class as 🎯T30, surviving
// on the one branch that never took `landed` evidence.
//
// Two things are pinned here. A pane that is merely slow must not be
// read as a pane that never received the payload, and a pane that really
// never receives it must still fail — loudly, quoting what it saw.

// t101LoadedPaneFrames returns the measured live sequence: the composer
// idle and empty for longer than the old two-sample bound, then the
// spinner. Both frames are verbatim captures from the 🎯T101 probe.
func t101LoadedPaneFrames(t *testing.T, emptySamples int) [][]byte {
	t.Helper()
	idle := loadFrame(t, "frame_t101_idle_before_echo.txt")
	chrome := loadFrame(t, "frame_t101_chrome_after_echo.txt")
	frames := make([][]byte, 0, emptySamples+1)
	for range emptySamples {
		frames = append(frames, idle)
	}
	return append(frames, chrome)
}

// TestT101ShortSendSurvivesLateTurnChrome is the regression oracle: a
// short typed send whose echo and chrome both arrive after the old
// 800ms bound must succeed.
func TestT101ShortSendSurvivesLateTurnChrome(t *testing.T) {
	t.Parallel()
	// One frame for the not-connecting check, then five idle samples —
	// 2s of polling, well past the two samples the old loop allowed.
	const emptySamples = 6
	frames := t101LoadedPaneFrames(t, emptySamples)
	i := 0
	d, enters := hermeticDriver(
		func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return f, nil
		},
		func(string) error { t.Fatal("short single-line message must not paste"); return nil },
		func(string) error { return nil },
	)
	start := d.clock()
	if err := sendKeysWith(d, "respond with: ok"); err != nil {
		t.Fatalf("SendKeys refused a send the pane was merely slow to show: %v", err)
	}
	if elapsed := d.clock().Sub(start); elapsed <= 2*submitSettle {
		t.Errorf("gave up looking after %s; the old bound was %s and that is what 🎯T101 failed on",
			elapsed, 2*submitSettle)
	}
	// An Enter into a composer whose contents the TUI has not drawn yet
	// can only land as a second, empty submit. One Enter, no hammering.
	if *enters != 1 {
		t.Errorf("enters=%d, want 1: an idle empty composer with nothing seen in it must not be re-pressed", *enters)
	}
}

// TestT101UnsubmittedSendStillFailsLoudly is the over-broadness guard.
// Waiting longer must not turn a genuinely lost payload into a silent
// success: a pane that never shows the brief and never starts a turn
// still errors, and the error quotes the frame it gave up on.
func TestT101UnsubmittedSendStillFailsLoudly(t *testing.T) {
	t.Parallel()
	idle := loadFrame(t, "frame_t101_idle_before_echo.txt")
	d, _ := hermeticDriver(
		func() ([]byte, error) { return idle, nil },
		func(string) error { return nil },
		func(string) error { return nil },
	)
	start := d.clock()
	err := sendKeysWith(d, "respond with: ok")
	if err == nil {
		t.Fatal("want an error when the payload is never seen and no turn begins")
	}
	if !strings.Contains(err.Error(), "turn not submitted") {
		t.Errorf("error should name the unsubmitted turn: %v", err)
	}
	if !strings.Contains(err.Error(), "brief never reached pane") {
		t.Errorf("error should keep the verdict consumers match on: %v", err)
	}
	// 🎯T91: report what it SAW, not only how long it waited.
	if !strings.Contains(err.Error(), "bypass permissions on") {
		t.Errorf("error should quote the frame it gave up on: %v", err)
	}
	if elapsed := d.clock().Sub(start); elapsed < submitEvidenceTimeout {
		t.Errorf("gave up after %s, want the full evidence bound %s", elapsed, submitEvidenceTimeout)
	}
}

// TestT101TypedBranchWaitsForTheEchoBeforeEnter pins the structural half
// of the fix. The typed branch used to fire Enter blind, immediately
// after send-keys -l, and so could never record that it had SEEN the
// payload in the box — which is why an empty box afterwards had to be
// read as "never arrived" (🎯T30's ambiguity, surviving on the one
// branch that never resolved it). It now waits for the echo, exactly as
// the paste branch waits for the chip, and that sighting is what makes
// the empty box the payload leaves behind read as "submitted".
func TestT101TypedBranchWaitsForTheEchoBeforeEnter(t *testing.T) {
	t.Parallel()
	idle := loadFrame(t, "frame_t101_idle_before_echo.txt")
	chrome := loadFrame(t, "frame_t101_chrome_after_echo.txt")
	// The not-connecting check reads the first frame; the echo appears
	// two captures later, the way a loaded pane delivers it.
	frames := [][]byte{idle, idle, []byte(typedNotSubmittedFrame), chrome}
	captures := 0
	enterAfterCaptures := -1
	virtualNow := time.Now()
	d := sendDriver{
		typeLiteral: func(string) error { return nil },
		pasteBuffer: func(string) error { t.Fatal("short single-line message must not paste"); return nil },
		sendEnter: func() error {
			if enterAfterCaptures < 0 {
				enterAfterCaptures = captures
			}
			return nil
		},
		capture: func() ([]byte, error) {
			f := frames[min(captures, len(frames)-1)]
			captures++
			return f, nil
		},
		sleep: func(d time.Duration) { virtualNow = virtualNow.Add(d) },
		now:   func() time.Time { return virtualNow },
	}
	if err := sendKeysWith(d, "summarise the build failure"); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	if enterAfterCaptures < 3 {
		t.Errorf("Enter fired after %d captures, before the payload was visible at capture 3: "+
			"a blind Enter leaves the typed branch with no evidence its payload ever landed",
			enterAfterCaptures)
	}
}

// TestT101SlowEchoIsNotAFailure: the typed branch waits for the echo,
// but a TUI too loaded to echo within contentLandTimeout must not fail
// the send there. The keystrokes are already queued in the pty; only the
// evidence is missing, and the submit loop still confirms from chrome.
func TestT101SlowEchoIsNotAFailure(t *testing.T) {
	t.Parallel()
	idle := loadFrame(t, "frame_t101_idle_before_echo.txt")
	chrome := loadFrame(t, "frame_t101_chrome_after_echo.txt")
	// Idle right through the whole content-land wait, then chrome.
	samples := int(contentLandTimeout/submitSettle) + 4
	frames := make([][]byte, 0, samples+1)
	for range samples {
		frames = append(frames, idle)
	}
	frames = append(frames, chrome)
	i := 0
	d, _ := hermeticDriver(
		func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return f, nil
		},
		func(string) error { return nil },
		func(string) error { return nil },
	)
	if err := sendKeysWith(d, "respond with: ok"); err != nil {
		t.Fatalf("a missing echo is missing evidence, not a failed send: %v", err)
	}
}

// TestT101EvidenceBoundOutlastsMeasuredPaneLatency ties the constant to
// the measurement rather than to taste: the bound must cover the worst
// pane latency 🎯T101 recorded on the live path, with room over it.
func TestT101EvidenceBoundOutlastsMeasuredPaneLatency(t *testing.T) {
	t.Parallel()
	// Measured on the live path at load average ~200: 2.4s from
	// send-keys -l to the echo, 1.6s from Enter to the first spinner.
	const measuredPaneLatency = 2400 * time.Millisecond
	if submitEvidenceTimeout <= measuredPaneLatency {
		t.Fatalf("submitEvidenceTimeout=%s does not cover the measured pane latency %s",
			submitEvidenceTimeout, measuredPaneLatency)
	}
	if maxSubmitPresses*submitSettle > submitEvidenceTimeout {
		t.Fatalf("press bound %s exceeds the evidence bound %s: presses would decide what the clock should",
			maxSubmitPresses*submitSettle, submitEvidenceTimeout)
	}
}

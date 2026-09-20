// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"os"
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
// (other startup menus). Trust-folder chrome is a separate fixture —
// current Claude copy has no ❯ N. cursor (🎯T87).
const numberedMenuCursorOnly = `  Choose an option:

  ❯ 1. Yes, proceed
    2. No

  Press Enter to confirm`

// trustFolderCursorUnnumbered: current chrome with a ❯ on the accept
// line but no digit — startupMenuCursor requires ❯ N. and misses it.
const trustFolderCursorUnnumbered = `  Quick safety check: Is this a project you created or one you trust?

  ❯ Yes, I trust this folder
    No, exit

  Enter to confirm · Esc to cancel`

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
		{"trust folder, unnumbered cursor", trustFolderCursorUnnumbered, false, true},
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

// TestMatchTrustFolderFixture is the 🎯T87 oracle: a first-open
// workspace-trust pane capture must be classified, and the classification
// must come from the trust copy — not from the pre-T87 signals
// (numbered ❯ N. cursor / resume wording). A fixture that already
// matches those would be green on the broken tree.
func TestMatchTrustFolderFixture(t *testing.T) {
	frame := loadFrame(t, "frame_trust_folder.txt")
	if MatchReady(frame) {
		t.Fatal("trust-folder chrome is not a live composer")
	}
	if startupMenuCursor.Match(trimTrailingSpace(frame)) {
		t.Fatal("fixture matches startupMenuCursor; this oracle would pass before 🎯T87")
	}
	if resumePrompt.Match(trimTrailingSpace(frame)) {
		t.Fatal("fixture matches resumePrompt; this oracle would pass before 🎯T87")
	}
	if !MatchTrustFolder(frame) {
		t.Fatal("MatchTrustFolder = false on current Claude trust-folder chrome")
	}
	if !MatchStartupMenu(frame) {
		t.Fatal("MatchStartupMenu must treat trust-folder as a dismissible startup menu")
	}
	if got := NotReadyReason(frame); got != NotReadyNoComposer {
		t.Fatalf("NotReadyReason = %q, want %s (no composer until the dialog is dismissed)", got, NotReadyNoComposer)
	}

	questionOnly := []byte("Quick safety check: ignore this, it is transcript prose.")
	if MatchTrustFolder(questionOnly) || MatchStartupMenu(questionOnly) {
		t.Fatal("question copy alone is not a trust dialog")
	}
	acceptOnly := []byte("the default is Yes, I trust this folder")
	if MatchTrustFolder(acceptOnly) || MatchStartupMenu(acceptOnly) {
		t.Fatal("accept-option copy alone is not a trust dialog")
	}
}

// TestWaitReadyAutoAdvancesTrustFolder: a first-open owner workdir that
// parks on the trust dialog must reach ready by the loop pressing Enter,
// bounded by maxMenuDismissals.
func TestWaitReadyAutoAdvancesTrustFolder(t *testing.T) {
	trust := loadFrame(t, "frame_trust_folder.txt")
	frames := [][]byte{
		trust,                // 1st capture: trust dialog → auto-Enter
		trust,                // still repainting → auto-Enter again
		[]byte(idleBoxFrame), // dialog cleared → ready
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
		t.Fatalf("waitReadyLoop wedged on trust-folder instead of auto-advancing: %v", err)
	}
	if enters == 0 {
		t.Fatal("loop reached ready but never pressed Enter — trust dialog was not auto-confirmed")
	}
	if enters > maxMenuDismissals {
		t.Fatalf("pressed Enter %d time(s), exceeds maxMenuDismissals=%d", enters, maxMenuDismissals)
	}
	t.Logf("auto-advanced through trust-folder in %s with %d Enter(s)", elapsed.Round(time.Millisecond), enters)
}

// TestWaitReadyTrustThenSplashThenReady: after trust dismiss, the TUI
// may paint the startup splash before the live composer. Same contract
// as the resume-menu path — no Enter into a dead box.
func TestWaitReadyTrustThenSplashThenReady(t *testing.T) {
	frames := [][]byte{
		loadFrame(t, "frame_trust_folder.txt"),
		[]byte(startupSplashFrame),
		[]byte(startupSplashFrame),
		[]byte(liveComposerFrame),
	}
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
		t.Fatalf("waitReadyLoop: %v", err)
	}
	if enters != 1 {
		t.Fatalf("Enter presses = %d, want 1 (trust dialog only; splash must not be auto-confirmed)", enters)
	}
	if i < 4 {
		t.Errorf("returned after %d capture(s); must poll trust→splash→live", i)
	}
}

// TestWaitReadyTrustTimeoutStaysBounded: Enter that never clears the
// trust dialog must stop at maxMenuDismissals with the wedged-menu
// error, not press forever and not fall through to generic no_composer.
func TestWaitReadyTrustTimeoutStaysBounded(t *testing.T) {
	trust := loadFrame(t, "frame_trust_folder.txt")
	enters := 0
	d := readyDriver{
		capture:   func() ([]byte, error) { return trust, nil },
		sendEnter: func() error { enters++; return nil },
	}

	_, err := waitReadyLoop(d, time.Millisecond, 100*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error when the trust dialog never clears")
	}
	if !strings.Contains(err.Error(), "startup menu") {
		t.Fatalf("error should name the wedged menu, got: %v", err)
	}
	if !strings.Contains(err.Error(), "trust-folder") {
		t.Fatalf("error should name the trust-folder prompt, got: %v", err)
	}
	if enters != maxMenuDismissals {
		t.Fatalf("expected exactly %d auto-confirmations, got %d", maxMenuDismissals, enters)
	}
}

// TestWaitReadyResumeThenTrustThenReady: a resume menu may be followed
// by the trust-folder dialog (the case maxMenuDismissals was sized
// for). Both are confirmed; the live composer is not.
func TestWaitReadyResumeThenTrustThenReady(t *testing.T) {
	frames := [][]byte{
		[]byte(resumeMenuFrame),
		loadFrame(t, "frame_trust_folder.txt"),
		[]byte(liveComposerFrame),
	}
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
		t.Fatalf("waitReadyLoop: %v", err)
	}
	if enters != 2 {
		t.Fatalf("Enter presses = %d, want 2 (resume then trust; composer is not a menu)", enters)
	}
	if enters > maxMenuDismissals {
		t.Fatalf("pressed Enter %d time(s), exceeds maxMenuDismissals=%d", enters, maxMenuDismissals)
	}
}

// TestWaitReadyDoesNotEnterOnTrustMentionInTranscript: overseer prose
// that names the dialog is not the dialog (jevons 🎯T565 class).
func TestWaitReadyDoesNotEnterOnTrustMentionInTranscript(t *testing.T) {
	frame := []byte("" +
		"the seat hit Quick safety check and never drew a composer\n" +
		liveComposerFrame)
	enters := 0
	d := readyDriver{
		capture:   func() ([]byte, error) { return frame, nil },
		sendEnter: func() error { enters++; return nil },
	}
	if _, err := waitReadyLoop(d, time.Millisecond, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitReadyLoop: %v", err)
	}
	if enters != 0 {
		t.Fatalf("pressed Enter %d time(s) into a live composer whose transcript mentions the trust dialog", enters)
	}
	if MatchTrustFolder(frame) {
		t.Fatal("question copy in transcript without the accept option must not classify as trust-folder")
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
		// A turn is running and a multi-row brief is sitting in the box
		// unsubmitted (🎯T28); the box is live and accepting more input.
		"frame_brief_stuck_during_turn.txt",
		"frame_brief_stuck_scrolled.txt",
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
		"trust folder, unnumbered cursor":                  trustFolderCursorUnnumbered,
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

// Claude Code prints a warning per permission rule that names no tool
// before its TUI mounts. A composer below those lines is a live composer
// (the pattern anchors on the tail), and a frame that is only the warnings
// is not a match — and its timeout names the frame verbatim and says the
// warnings are not the cause (jevons 🎯T565).
const settingsWarningsFrame = "Permission deny rule \"TeamCreate\" matches no known tool — check for typos.\n" +
	"Permission deny rule \"TeamDelete\" matches no known tool — check for typos.\n\n\n\n\n\n"

func TestMatchReadyToleratesSettingsWarnings(t *testing.T) {
	frame := []byte(settingsWarningsFrame + "\n" + idleBoxFrame)
	if !MatchReady(frame) {
		t.Fatalf("composer under settings warnings must be ready:\n%s", frame)
	}
	if MatchStartupWarningsOnly(frame) {
		t.Fatal("a frame with a composer is not warnings-only")
	}
	if !MatchStartupWarningsOnly([]byte(settingsWarningsFrame)) {
		t.Fatal("warnings + blank lines is warnings-only")
	}
	if MatchReady([]byte(settingsWarningsFrame)) {
		t.Fatal("warnings alone are not a ready verdict")
	}
}

func TestWaitReadyWarningsOnlyTimeoutNamesFrameVerbatim(t *testing.T) {
	d := readyDriver{
		capture:   func() ([]byte, error) { return []byte(settingsWarningsFrame), nil },
		sendEnter: func() error { t.Fatal("no menu to dismiss"); return nil },
	}
	_, err := waitReadyLoop(d, time.Millisecond, 50*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	msg := err.Error()
	if !strings.Contains(msg, "startup settings warnings") || !strings.Contains(msg, "not the cause") {
		t.Fatalf("timeout must diagnose a warnings-only frame, got: %v", err)
	}
	if !strings.Contains(msg, settingsWarningsFrame) {
		t.Fatalf("timeout must carry the last frame verbatim, got: %v", err)
	}
}

// A transcript that talks about "/rc connecting" is not a status bar
// that says it. The overseer's own diagnosis of a wedge wedged its pane
// for twenty consecutive deliveries (jevons 🎯T565, 2026-08-29).
func TestMatchConnectingReadsOnlyTheStatusTail(t *testing.T) {
	mention, err := os.ReadFile("testdata/frame_rc_mentioned_in_transcript.txt")
	if err != nil {
		t.Fatal(err)
	}
	if MatchConnecting(mention) {
		t.Fatalf("transcript prose mentioning /rc connecting read as connecting")
	}
	if !MatchReady(mention) {
		t.Fatalf("idle composer under an /rc status bar must be ready")
	}
	status, err := os.ReadFile("testdata/frame_rc_connecting_status.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !MatchConnecting(status) {
		t.Fatalf("status bar showing /rc connecting… must still read as connecting")
	}
	if MatchReady(status) {
		t.Fatalf("connecting frame must not be ready")
	}
}

func TestNotReadyReasonTokens(t *testing.T) {
	t.Parallel()
	splashNoRC := strings.ReplaceAll(startupSplashFrame, " /rc connecting…", "")
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{"connecting status", connectingFrame, NotReadyRCConnecting},
		{"warnings only", settingsWarningsFrame, NotReadySettingsWarning},
		{"splash without /rc connecting", splashNoRC, NotReadySplash},
		{"streaming, no box", streamingFrame, NotReadyNoComposer},
		{"live composer", liveComposerFrame, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NotReadyReason([]byte(c.frame)); got != c.want {
				t.Fatalf("NotReadyReason=%q want %q", got, c.want)
			}
		})
	}
}

func TestWaitReadyTimeoutNamesReason(t *testing.T) {
	t.Parallel()
	splashNoRC := strings.ReplaceAll(startupSplashFrame, " /rc connecting…", "")
	cases := []struct {
		name  string
		frame string
		token string
	}{
		{"rc_connecting", connectingFrame, NotReadyRCConnecting},
		{"settings_warning", settingsWarningsFrame, NotReadySettingsWarning},
		{"splash", splashNoRC, NotReadySplash},
		{"no_composer", streamingFrame, NotReadyNoComposer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := readyDriver{
				capture:   func() ([]byte, error) { return []byte(c.frame), nil },
				sendEnter: func() error { t.Fatal("no menu to dismiss"); return nil },
			}
			_, err := waitReadyLoop(d, time.Millisecond, 40*time.Millisecond, time.Millisecond)
			if err == nil {
				t.Fatal("expected a timeout")
			}
			msg := err.Error()
			if !strings.Contains(msg, "claude not ready ("+c.token+")") {
				t.Fatalf("timeout must name %s, got: %v", c.token, err)
			}
			if strings.Contains(msg, "ready pattern did not match") {
				t.Fatalf("generic pattern message must not be used when the reason is known: %v", err)
			}
		})
	}
}

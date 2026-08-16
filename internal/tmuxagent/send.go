// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// pasteBlockThreshold: messages at or above this size (or containing a
// newline) use load-buffer + bracketed paste rather than send-keys -l.
// Large literal send-keys floods land as Claude Code's collapsed
// "[Pasted text #N +X lines] / paste again to expand" chips; a single
// trailing CR often never submits the turn (🎯T305 Failure B).
//
// This is a ROUTING threshold, not a size limit, and SendKeys has no
// maximum submittable size: a caller does not need to chunk (🎯T30). The
// bound the paste branch is proven to on the live path is 6400 bytes in
// one send, both into an idle composer and mid-turn — see
// TestT30LargePayloadSubmitsOnRealPath in the root package, and
// TestT30LiveReproSendSucceeds for the captured frames. What made size
// look like a limit was classification, not tmux or the CLI: the paste
// branch is the only one that produces a collapsed chip and the
// lingering "paste again to expand" footer, and both defects 🎯T30 and
// 🎯T28 closed were misreadings of that UI, so exactly the sends big
// enough to be pasted were the sends reported as failures.
const pasteBlockThreshold = 400

const (
	// maxSubmitPresses bounds how many Enter keys we press after paste
	// while the composer still shows an unsubmitted paste chip.
	maxSubmitPresses = 8
	// submitSettle is how long to wait between capture-and-retry cycles
	// so Claude Code can render the paste chip / leave the idle box.
	// Live evidence (f8f9a4cf): 150ms was too short while /rc connecting
	// and paste chips need ~300–500ms to accept Enter.
	submitSettle = 400 * time.Millisecond
	// contentLandTimeout waits for paste/type to appear before the first
	// Enter (avoids Enter-into-empty during paste render).
	contentLandTimeout = 3 * time.Second
	// connectingClearTimeout waits for /rc connecting to clear before
	// paste when the ready channel closed on a still-connecting frame
	// (older agents); MatchReady now rejects connecting too.
	connectingClearTimeout = 15 * time.Second
)

// pastedTextChip matches Claude Code's collapsed paste chip. Prefer the
// "[Pasted text #N" form; "paste again to expand" alone is a weaker
// residual that may linger in the viewport.
var (
	pastedTextChip  = regexp.MustCompile(`(?i)\[Pasted\s*text\s*#\d+`)
	pasteExpandHint = regexp.MustCompile(`(?i)paste\s+again\s+to\s+expand`)
	// spinnerLine matches Claude Code's activity line — one of its cycling
	// spinner glyphs, then the gerund it is currently showing, ending in an
	// ellipsis ("✶ Pollinating…"). Naming a single glyph is not enough: the
	// CLI rotates through the whole set, and early in a turn the line
	// carries neither a timer nor a token count, while the footer row that
	// usually says "esc to interrupt" can be displaced by the paste hint.
	// A frame in exactly that state — spinner up, composer drained — was
	// reported as "composer empty after paste (brief never reached pane)"
	// for a payload the agent had already taken up (🎯T28 live capture,
	// testdata/frame_daily_spinner_empty_composer.txt).
	spinnerLine        = regexp.MustCompile(`(?m)^[^\S\n]*[·✢✳∗✱✲✳✴✵✶✷✸✹✺✻✽❋*][^\S\n]+\S[^\n]*…`)
	turnInProgressHint = regexp.MustCompile(`(?i)(burrowing|thinking|esc to interrupt|cooking|blanching|still thinking|✳|running stop|tokens)`)
	// queuedMessagesHint is Claude Code's ghost hint in an otherwise
	// empty composer, drawn only while at least one message is queued
	// behind the running turn. It is the CLI's own statement that a
	// mid-turn message was ACCEPTED: the composer is vacated the instant
	// a message queues (🎯T28 live capture,
	// testdata/frame_queued_hint_during_turn.txt).
	queuedMessagesHint = regexp.MustCompile(`(?i)press up to edit queued message`)
)

// MatchUnsubmittedPaste reports whether the frame still holds a Claude
// Code paste chip that has not been submitted as a turn. Used by Send
// to press through the block and by hosts that confirm delivery.
func MatchUnsubmittedPaste(frame []byte) bool {
	f := trimTrailingSpace(frame)
	if pastedTextChip.Match(f) {
		return true
	}
	// "paste again to expand" alone is a weak residual: Claude Code keeps
	// it in the footer for a while after the paste it describes has been
	// submitted (🎯T28 live probe run 2 — a payload that had queued
	// cleanly read as a stuck chip, so Send failed a send that landed).
	// It therefore only counts while the composer holds something
	// unsubmitted of its own.
	return pasteExpandHint.Match(f) && composerHoldsUnsubmitted(frame)
}

// composerHoldsUnsubmitted reports whether the live composer box at the
// tail of the frame holds text that has not been submitted. The ghost
// hints Claude Code draws inside an otherwise empty box — the startup
// placeholder and the queued-messages hint — are chrome, not content.
func composerHoldsUnsubmitted(frame []byte) bool {
	body := composerBody(frame)
	if body == nil {
		return false
	}
	if startupPlaceholder.Match(body) || queuedMessagesHint.Match(body) {
		return false
	}
	return len(strings.TrimSpace(string(body))) > 0
}

// MatchTurnInProgress reports Claude Code actively running a turn
// (spinner / thinking chrome), independent of the idle composer pattern.
func MatchTurnInProgress(frame []byte) bool {
	f := trimTrailingSpace(frame)
	return turnInProgressHint.Match(f) || spinnerLine.Match(f)
}

// SendKeys delivers msg to the target window and submits the turn.
//
// Short single-line messages are typed with send-keys -l (preserves the
// historical fast path). Multi-line or large messages go through
// load-buffer + paste-buffer -p -r so Claude Code receives one bracketed
// paste instead of a keystroke flood.
//
// After paste, Enter is sent as a named key (not literal -l CR). If a
// collapsed paste chip remains, additional Enter presses are issued
// (bounded). SendKeys returns an error when the turn never begins —
// silent "keys sent, turn never started" is banned (🎯T305).
func SendKeys(windowID, msg string) error {
	return sendKeysWith(defaultSendDriver(windowID), msg)
}

// sendDriver abstracts tmux side effects so submit/paste logic is hermetic.
type sendDriver struct {
	typeLiteral func(msg string) error
	pasteBuffer func(msg string) error
	sendEnter   func() error
	capture     func() ([]byte, error)
	sleep       func(time.Duration)
	// now is optional; tests inject a virtual clock that advances with sleep.
	now func() time.Time
}

func defaultSendDriver(windowID string) sendDriver {
	return sendDriver{
		typeLiteral: func(msg string) error { return typeLiteral(windowID, msg) },
		pasteBuffer: func(msg string) error { return pasteViaBuffer(windowID, msg) },
		sendEnter:   func() error { return sendNamedEnter(windowID) },
		capture:     func() ([]byte, error) { return CapturePane(windowID) },
		sleep:       time.Sleep,
		now:         time.Now,
	}
}

func (d *sendDriver) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func sendKeysWith(d sendDriver, msg string) error {
	if d.sleep == nil {
		d.sleep = time.Sleep
	}
	if d.now == nil {
		d.now = time.Now
	}
	// Belt-and-suspenders: never paste while /rc connecting (MatchReady
	// also rejects it, but older ready channels may already be closed).
	if err := waitNotConnecting(d, connectingClearTimeout); err != nil {
		return err
	}

	// landed records that this send's payload was SEEN in the composer
	// before Enter. It is the difference between an empty box that never
	// received the brief and an empty box the brief left when it
	// submitted, which is the whole of 🎯T30 (see ensureSubmitted).
	landed := false
	if msg != "" {
		if useBracketedPaste(msg) {
			if err := d.pasteBuffer(msg); err != nil {
				return err
			}
			// Wait for the chip / body to appear before Enter.
			if err := waitContentLanded(d, contentLandTimeout); err != nil {
				return err
			}
			landed = true
		} else {
			if err := d.typeLiteral(msg); err != nil {
				return err
			}
		}
	}
	// Empty msg: bare Enter (used by readiness menu dismiss).
	if err := d.sendEnter(); err != nil {
		return err
	}
	if msg == "" {
		return nil
	}
	return ensureSubmitted(d, landed)
}

// useBracketedPaste chooses the paste-buffer path for multi-line or
// large briefs that would otherwise collapse into paste chips without
// a reliable submit.
func useBracketedPaste(msg string) bool {
	return len(msg) >= pasteBlockThreshold || strings.Contains(msg, "\n")
}

// composerState classifies the post-Send pane for delivery confirmation.
type composerState int

const (
	composerUnknown composerState = iota
	// composerWorking: positive turn evidence — Claude is running a turn
	// (or has just run one) and the composer is not holding anything
	// back, so the brief was submitted.
	composerWorking
	// composerQueued: the composer shows Claude Code's queued-messages
	// ghost hint. The brief was accepted behind a running turn and will
	// be delivered at the turn boundary — submitted, by the CLI's own
	// account (🎯T28).
	composerQueued
	// composerPasteChip: collapsed paste still holding the brief.
	composerPasteChip
	// composerTyped: idle box still holds text (typed/expanded, not sent).
	composerTyped
	// composerEmptyIdle: idle empty box — brief never landed or was cleared.
	composerEmptyIdle
	// composerUnrecognised: the frame shows neither turn chrome nor the
	// idle composer at the pane tail. A startup/resume menu, a dialog, a
	// multi-line composer holding an unsubmitted brief, scrolled output,
	// a partial redraw, and the post-Enter window before spinner chrome
	// all land here. Absence of the idle box is NOT evidence the turn
	// began (🎯T21) — the caller must keep looking.
	composerUnrecognised
)

// classifyComposer is the pure post-Send oracle for 🎯T305 delivery
// confirmation. The turn-began verdict requires POSITIVE evidence:
// Claude Code's own turn chrome (spinner / "esc to interrupt" / token
// counter), which is also what a frame still shows just after a turn
// completes. Everything that merely fails to look like the idle
// composer is composerUnrecognised, never working (🎯T21).
//
// The composer is read BEFORE the chrome (🎯T28). Turn chrome is
// evidence that SOME turn is running, not that THIS send started one:
// when a turn was already in flight at send time — the ordinary case
// for a fleet that briefs a busy agent — chrome is on the pane from the
// moment the payload arrives, so letting it win means a payload sitting
// unsubmitted in the composer is reported as delivered, with zero
// verification (the whole 🎯T28 symptom; see
// testdata/frame_t28_probe.txt). Text in the box is unsubmitted text,
// chrome or no chrome; a message that really queued behind the turn
// vacates the box and leaves the queued-messages hint instead.
func classifyComposer(frame []byte) composerState {
	if frame == nil {
		return composerUnknown
	}
	if MatchUnsubmittedPaste(frame) {
		return composerPasteChip
	}
	body := composerBody(frame)
	switch {
	case body == nil:
		// No live composer at the tail. Chrome is the only remaining
		// evidence, and nothing is visibly being held back.
		if MatchTurnInProgress(frame) {
			return composerWorking
		}
		return composerUnrecognised
	case queuedMessagesHint.Match(body):
		return composerQueued
	case startupPlaceholder.Match(body):
		// Box drawn but input not yet wired (🎯T284): not ready, and
		// certainly not a turn.
		return composerUnrecognised
	case len(strings.TrimSpace(string(body))) > 0:
		return composerTyped
	case MatchTurnInProgress(frame):
		return composerWorking
	case !MatchReady(frame):
		// Empty box that cannot accept a turn (/rc connecting).
		return composerUnrecognised
	default:
		return composerEmptyIdle
	}
}

// submitted reports whether the state is positive evidence that this
// send's payload left the composer: a turn running with nothing held
// back, or Claude Code's own queued-messages hint.
func (s composerState) submitted() bool {
	return s == composerWorking || s == composerQueued
}

// waitNotConnecting polls until /rc connecting clears (or timeout).
func waitNotConnecting(d sendDriver, timeout time.Duration) error {
	if d.capture == nil {
		return nil
	}
	deadline := d.clock().Add(timeout)
	for {
		frame, err := d.capture()
		if err == nil && !MatchConnecting(frame) {
			return nil
		}
		if !d.clock().Before(deadline) {
			return fmt.Errorf("claude still /rc connecting after %s; refusing to paste", timeout)
		}
		d.sleep(submitSettle)
	}
}

// waitContentLanded waits until the pasted brief is visibly in the
// composer (chip or typed body). An unrecognised frame is not proof the
// paste rendered — firing Enter on it can submit nothing at all
// (🎯T21) — so the loop keeps polling and reports the state it was
// stuck in when the timeout expires.
//
// Turn chrome does NOT satisfy this wait (🎯T28): pasting into an agent
// that is mid-turn leaves the chrome of the RUNNING turn on the pane
// from the first capture, so accepting it fired Enter before the paste
// existed and made the whole submit loop a no-op.
func waitContentLanded(d sendDriver, timeout time.Duration) error {
	if d.capture == nil {
		return nil
	}
	deadline := d.clock().Add(timeout)
	var last []byte
	lastState := composerUnknown
	for {
		frame, err := d.capture()
		if err == nil {
			last = frame
			lastState = classifyComposer(frame)
			if lastState == composerPasteChip || lastState == composerTyped {
				return nil
			}
		}
		if !d.clock().Before(deadline) {
			return fmt.Errorf("turn not submitted: paste never appeared in pane within %s; composer state=%s; last frame tail:\n%s",
				timeout, composerStateName(lastState), frameTail(last))
		}
		d.sleep(submitSettle)
	}
}

// ensureSubmitted re-presses Enter while the composer still holds an
// unsubmitted brief (paste chip or typed text). Returns nil only on
// positive evidence that THIS send's payload left the composer: a
// running turn with nothing held back, the queued-messages hint
// (🎯T28), or the box the payload was seen in going empty under our own
// Enter (🎯T30, below). Errors if the chip/text remains, or if the
// composer is empty for a payload that was never seen landing.
//
// landed says the caller already watched this payload render in the box
// (waitContentLanded's postcondition on the paste branch). Without it
// the loop starts blind — sawContent false — and reads the empty box a
// successfully submitted paste leaves behind as "brief never reached
// pane", failing a send whose payload the model has already answered.
// That is 🎯T30's false refusal, and it is size-selective in the way the
// target describes: only a payload at or above pasteBlockThreshold takes
// the bracketed-paste branch that vacates the box under one Enter, so
// sub-threshold sends pass and every finish-report-sized one is refused.
// Live evidence, one send apart, replayed by
// TestT30LiveReproSendSucceeds: testdata/frame_t30_chip_before_enter.txt
// and testdata/frame_t30_spinner_drained_after_enter.txt.
func ensureSubmitted(d sendDriver, landed bool) error {
	if d.capture == nil {
		return nil
	}
	if d.sleep == nil {
		d.sleep = time.Sleep
	}
	var last []byte
	var lastState composerState
	sawContent := landed
	for press := 0; press < maxSubmitPresses; press++ {
		d.sleep(submitSettle)
		frame, err := d.capture()
		if err != nil {
			// Capture glitches are non-fatal mid-loop; re-press and retry.
			_ = d.sendEnter()
			continue
		}
		last = frame
		lastState = classifyComposer(frame)
		if lastState.submitted() {
			return nil
		}
		switch lastState {
		case composerPasteChip, composerTyped:
			sawContent = true
			if press+1 < maxSubmitPresses {
				if err := d.sendEnter(); err != nil {
					return fmt.Errorf("tmux send-keys Enter (paste submit retry): %w", err)
				}
			}
		case composerUnrecognised:
			// Neither turn chrome nor the idle composer: the Enter may
			// have gone into a menu, a dialog, or a half-drawn frame.
			// Keep polling and re-pressing within the bound rather than
			// declaring the turn started (🎯T21).
			if press+1 < maxSubmitPresses {
				if err := d.sendEnter(); err != nil {
					return fmt.Errorf("tmux send-keys Enter (unrecognised frame retry): %w", err)
				}
			}
		case composerEmptyIdle:
			if !sawContent && press == 0 {
				// First sample empty — brief may still be rendering.
				continue
			}
			if !sawContent {
				return fmt.Errorf("turn not submitted: composer empty after paste (brief never reached pane)")
			}
			// The payload was in this box and is not any more, and the
			// only key we sent was Enter: it was submitted (🎯T30). This
			// is evidence about OUR payload, not about chrome that might
			// belong to another turn, so it is safe where the chrome-first
			// rule 🎯T28 removed was not. Pressing Enter again here is what
			// burned the whole bound and turned a delivered brief into
			// "turn not submitted".
			return nil
		default:
			if press+1 < maxSubmitPresses {
				_ = d.sendEnter()
			}
		}
	}
	return fmt.Errorf("turn not submitted: composer state=%s after %d Enter presses; last frame tail:\n%s",
		composerStateName(lastState), maxSubmitPresses, frameTail(last))
}

// frameTail renders the last frameTailBytes of a captured frame for
// failure reports (🎯T305 / 🎯T21), so the operator sees the UI the
// send loop was actually stuck on.
const frameTailBytes = 400

func frameTail(frame []byte) string {
	snippet := string(trimTrailingSpace(frame))
	if len(snippet) > frameTailBytes {
		snippet = snippet[len(snippet)-frameTailBytes:]
	}
	return snippet
}

func composerStateName(s composerState) string {
	switch s {
	case composerWorking:
		return "working"
	case composerQueued:
		return "queued_behind_turn"
	case composerPasteChip:
		return "paste_chip"
	case composerTyped:
		return "typed_unsubmitted"
	case composerEmptyIdle:
		return "empty_idle"
	case composerUnrecognised:
		return "unrecognised_no_turn_evidence"
	default:
		return "unknown"
	}
}

func typeLiteral(windowID, msg string) error {
	sock := SocketPath()
	if out, err := exec.Command(
		"tmux", "-S", sock,
		"send-keys", "-t", windowID, "-l", msg,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys (message): %w: %s", err, out)
	}
	return nil
}

// sendNamedEnter presses the Enter key by name (not send-keys -l "\r").
// Named Enter is what Claude Code's paste-chip UI responds to; a
// literal CR with -l is sometimes absorbed into the paste and never
// submits (🎯T305).
func sendNamedEnter(windowID string) error {
	sock := SocketPath()
	if out, err := exec.Command(
		"tmux", "-S", sock,
		"send-keys", "-t", windowID, "Enter",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys Enter: %w: %s", err, out)
	}
	return nil
}

// pasteViaBuffer writes msg to a temp file, load-buffers it, and
// paste-buffers with bracketed-paste (-p) and raw newlines (-r).
func pasteViaBuffer(windowID, msg string) error {
	sock := SocketPath()
	f, err := os.CreateTemp("", "claudia-send-*.txt")
	if err != nil {
		return fmt.Errorf("temp paste file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(msg); err != nil {
		f.Close()
		return fmt.Errorf("write paste file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close paste file: %w", err)
	}

	bufName := "claudia-send"
	if out, err := exec.Command(
		"tmux", "-S", sock,
		"load-buffer", "-b", bufName, path,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w: %s", err, out)
	}
	// -p: bracketed paste; -r: keep LFs (do not rewrite to CR); -d: drop buffer.
	if out, err := exec.Command(
		"tmux", "-S", sock,
		"paste-buffer", "-p", "-r", "-d", "-b", bufName, "-t", windowID,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux paste-buffer: %w: %s", err, out)
	}
	return nil
}

// SendEscape sends the Escape key (0x1b) to the target window.
// Matches Agent.Interrupt's semantics of cancelling the current turn.
// Uses send-keys without -l so "Escape" is interpreted as the key
// name rather than typed literally.
func SendEscape(windowID string) error {
	sock := SocketPath()
	if out, err := exec.Command(
		"tmux", "-S", sock,
		"send-keys", "-t", windowID, "Escape",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys Escape: %w: %s", err, out)
	}
	return nil
}

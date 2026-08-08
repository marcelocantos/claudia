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
	pastedTextChip     = regexp.MustCompile(`(?i)\[Pasted\s*text\s*#\d+`)
	pasteExpandHint    = regexp.MustCompile(`(?i)paste\s+again\s+to\s+expand`)
	turnInProgressHint = regexp.MustCompile(`(?i)(burrowing|thinking|esc to interrupt|cooking|blanching|still thinking|✳|running stop|tokens)`)
)

// MatchUnsubmittedPaste reports whether the frame still holds a Claude
// Code paste chip that has not been submitted as a turn. Used by Send
// to press through the block and by hosts that confirm delivery.
func MatchUnsubmittedPaste(frame []byte) bool {
	f := trimTrailingSpace(frame)
	if pastedTextChip.Match(f) {
		return true
	}
	// Expand hint alone only counts while the idle composer is still up
	// (working UIs may leave residual scrollback text).
	return pasteExpandHint.Match(f) && MatchReady(frame)
}

// MatchTurnInProgress reports Claude Code actively running a turn
// (spinner / thinking chrome), independent of the idle composer pattern.
func MatchTurnInProgress(frame []byte) bool {
	return turnInProgressHint.Match(trimTrailingSpace(frame))
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

	if msg != "" {
		if useBracketedPaste(msg) {
			if err := d.pasteBuffer(msg); err != nil {
				return err
			}
			// Wait for the chip / body to appear before Enter.
			if err := waitContentLanded(d, contentLandTimeout); err != nil {
				return err
			}
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
	return ensureSubmitted(d)
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
	// composerWorking: left the idle box — turn began (or non-idle UI).
	composerWorking
	// composerPasteChip: collapsed paste still holding the brief.
	composerPasteChip
	// composerTyped: idle box still holds text (typed/expanded, not sent).
	composerTyped
	// composerEmptyIdle: idle empty box — brief never landed or was cleared.
	composerEmptyIdle
)

// classifyComposer is the pure post-Send oracle for 🎯T305 delivery
// confirmation. Turn began when Claude shows working chrome or the idle
// single-line composer is gone without an active paste chip.
func classifyComposer(frame []byte) composerState {
	if frame == nil {
		return composerUnknown
	}
	// Working chrome wins even if residual "paste again" text lingers.
	if MatchTurnInProgress(frame) {
		return composerWorking
	}
	if MatchUnsubmittedPaste(frame) {
		return composerPasteChip
	}
	if !MatchReady(frame) {
		// No idle box and no paste chip → turn began (or non-idle UI).
		return composerWorking
	}
	body := composerBody(frame)
	if len(strings.TrimSpace(string(body))) == 0 {
		return composerEmptyIdle
	}
	return composerTyped
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

// waitContentLanded waits until paste chip or typed body appears.
func waitContentLanded(d sendDriver, timeout time.Duration) error {
	if d.capture == nil {
		return nil
	}
	deadline := d.clock().Add(timeout)
	for {
		frame, err := d.capture()
		if err == nil {
			st := classifyComposer(frame)
			if st == composerPasteChip || st == composerTyped || st == composerWorking {
				return nil
			}
		}
		if !d.clock().Before(deadline) {
			return fmt.Errorf("turn not submitted: paste never appeared in pane within %s", timeout)
		}
		d.sleep(submitSettle)
	}
}

// ensureSubmitted re-presses Enter while the composer still holds an
// unsubmitted brief (paste chip or typed text). Returns nil only when
// the turn begins. Errors if the chip/text remains or the composer is
// empty after paste (brief never landed).
func ensureSubmitted(d sendDriver) error {
	if d.capture == nil {
		return nil
	}
	if d.sleep == nil {
		d.sleep = time.Sleep
	}
	var last []byte
	var lastState composerState
	sawContent := false
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
		switch lastState {
		case composerWorking:
			return nil
		case composerPasteChip, composerTyped:
			sawContent = true
			if press+1 < maxSubmitPresses {
				if err := d.sendEnter(); err != nil {
					return fmt.Errorf("tmux send-keys Enter (paste submit retry): %w", err)
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
			// Content was seen then cleared without working chrome —
			// keep pressing Enter a few more times.
			if press+1 < maxSubmitPresses {
				_ = d.sendEnter()
			}
		default:
			if press+1 < maxSubmitPresses {
				_ = d.sendEnter()
			}
		}
	}
	snippet := string(trimTrailingSpace(last))
	if len(snippet) > 400 {
		snippet = snippet[len(snippet)-400:]
	}
	return fmt.Errorf("turn not submitted: composer state=%s after %d Enter presses; last frame tail:\n%s",
		composerStateName(lastState), maxSubmitPresses, snippet)
}

func composerStateName(s composerState) string {
	switch s {
	case composerWorking:
		return "working"
	case composerPasteChip:
		return "paste_chip"
	case composerTyped:
		return "typed_unsubmitted"
	case composerEmptyIdle:
		return "empty_idle"
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

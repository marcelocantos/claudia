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
	maxSubmitPresses = 4
	// submitSettle is how long to wait between capture-and-retry cycles
	// so Claude Code can render the paste chip / leave the idle box.
	submitSettle = 150 * time.Millisecond
)

// pastedTextChip matches Claude Code's collapsed paste chip in the
// composer or status area (spaces optional — capture-pane may insert
// CSI between tokens).
var pastedTextChip = regexp.MustCompile(`(?i)\[Pasted\s*text\s*#\d+|paste\s+again\s+to\s+expand`)

// MatchUnsubmittedPaste reports whether the frame still holds a Claude
// Code paste chip that has not been submitted as a turn. Used by Send
// to press through the block and by hosts that confirm delivery.
func MatchUnsubmittedPaste(frame []byte) bool {
	return pastedTextChip.Match(trimTrailingSpace(frame))
}

// SendKeys delivers msg to the target window and submits the turn.
//
// Short single-line messages are typed with send-keys -l (preserves the
// historical fast path). Multi-line or large messages go through
// load-buffer + paste-buffer -p -r so Claude Code receives one bracketed
// paste instead of a keystroke flood.
//
// After paste, Enter is sent as a named key (not literal -l CR). If the
// pane still shows a paste chip, additional Enter presses are issued
// (bounded). SendKeys returns an error when the paste remains
// unsubmitted — silent "keys sent, turn never started" is banned (🎯T305).
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
}

func defaultSendDriver(windowID string) sendDriver {
	return sendDriver{
		typeLiteral: func(msg string) error { return typeLiteral(windowID, msg) },
		pasteBuffer: func(msg string) error { return pasteViaBuffer(windowID, msg) },
		sendEnter:   func() error { return sendNamedEnter(windowID) },
		capture:     func() ([]byte, error) { return CapturePane(windowID) },
		sleep:       time.Sleep,
	}
}

func sendKeysWith(d sendDriver, msg string) error {
	if msg != "" {
		if useBracketedPaste(msg) {
			if err := d.pasteBuffer(msg); err != nil {
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
// confirmation. Turn began only when the pane left the idle composer
// (composerWorking). Paste chips and non-empty idle text are unsubmitted.
func classifyComposer(frame []byte) composerState {
	if frame == nil {
		return composerUnknown
	}
	if MatchUnsubmittedPaste(frame) {
		return composerPasteChip
	}
	if !MatchReady(frame) {
		return composerWorking
	}
	body := composerBody(frame)
	if len(strings.TrimSpace(string(body))) == 0 {
		return composerEmptyIdle
	}
	return composerTyped
}

// ensureSubmitted re-presses Enter while the composer still holds an
// unsubmitted brief (paste chip or typed text). Returns nil only when
// the pane leaves the idle box (turn began). Errors if the chip/text
// remains or the composer is empty after paste (brief never landed).
func ensureSubmitted(d sendDriver) error {
	if d.capture == nil {
		return nil
	}
	if d.sleep == nil {
		d.sleep = time.Sleep
	}
	var last []byte
	var lastState composerState
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
			if press+1 < maxSubmitPresses {
				if err := d.sendEnter(); err != nil {
					return fmt.Errorf("tmux send-keys Enter (paste submit retry): %w", err)
				}
			}
		case composerEmptyIdle:
			// Brief never appeared — more Enters will not help; fail loud.
			return fmt.Errorf("turn not submitted: composer empty after paste (brief never reached pane)")
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

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// CapturePane returns the rendered content of the given tmux window's
// active pane as a single blob via `tmux capture-pane -p`. Includes
// only the visible viewport (no scrollback), which is what the ready
// pattern matches against.
func CapturePane(windowID string) ([]byte, error) {
	sock := SocketPath()
	out, err := exec.Command("tmux", "-S", sock, "capture-pane", "-p", "-t", windowID).Output()
	if err != nil {
		return nil, fmt.Errorf("tmux capture-pane %s: %w", windowID, wrapExitErr(err))
	}
	return out, nil
}

// readyPattern matches Claude Code's idle input box near the bottom
// of the captured viewport: a horizontal rule made of ─ (U+2500,
// BOX DRAWINGS LIGHT HORIZONTAL), a line starting with the ❯ prompt
// glyph (U+276F, HEAVY RIGHT-POINTING ANGLE QUOTATION MARK ORNAMENT),
// and another horizontal rule. Up to 5 trailing lines are permitted
// between the bottom rule and end-of-frame so status lines like
// "⏵⏵ bypass permissions on (shift+tab to cycle)" don't break the
// match. \z anchors to end-of-input after trimTrailingSpace strips
// any trailing whitespace tmux appends.
//
// The ❯ line is matched with a permissive body ([^\n]*) rather than
// requiring an empty input area — that way we still detect
// readiness if the user has typed something into the input buffer
// and we haven't submitted yet. For startup readiness (the M1 case)
// the input is always empty.
var readyPattern = regexp.MustCompile(`─{10,}\n❯[^\n]*\n─{10,}(?:\n[^\n]*){0,5}\s*\z`)

// MatchReady reports whether the captured frame shows Claude's idle
// input box at the tail of the visible pane.
func MatchReady(frame []byte) bool {
	return readyPattern.Match(trimTrailingSpace(frame))
}

// trimTrailingSpace strips trailing whitespace so \z anchoring
// doesn't care about whatever newline/space tail tmux emits.
func trimTrailingSpace(b []byte) []byte {
	n := len(b)
	for n > 0 {
		switch b[n-1] {
		case ' ', '\t', '\n', '\r':
			n--
			continue
		}
		break
	}
	return b[:n]
}

// WaitReady polls capture-pane at `poll` intervals until MatchReady
// returns true or `timeout` elapses. Returns the elapsed time on
// success, or an error describing the last failure state on timeout.
//
// On timeout, if a capture-pane call ever succeeded the error
// includes the last captured frame so callers can diagnose why the
// ready pattern never matched (wrong glyph, truncated render, Claude
// hung in a tool-approval prompt).
func WaitReady(windowID string, poll, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(timeout)

	var lastFrame []byte
	var lastErr error

	for {
		frame, err := CapturePane(windowID)
		if err == nil {
			lastFrame = frame
			if MatchReady(frame) {
				return time.Since(start), nil
			}
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			if lastErr != nil && lastFrame == nil {
				return 0, fmt.Errorf("capture-pane never succeeded within %s: %w", timeout, lastErr)
			}
			return 0, fmt.Errorf("ready pattern did not match within %s; last frame:\n%s", timeout, lastFrame)
		}
		time.Sleep(poll)
	}
}

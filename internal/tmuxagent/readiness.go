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
// The ❯ line is matched with a permissive body rather than requiring
// an empty input area — that way we still detect readiness if the user
// has typed something into the input buffer and we haven't submitted
// yet. The body is captured so MatchReady can rule out the startup
// splash (see startupPlaceholder).
var readyPattern = regexp.MustCompile(`─{10,}\n❯([^\n]*)\n─{10,}(?:\n[^\n]*){0,5}\s*\z`)

// startupPlaceholder matches the ghost hint Claude Code renders inside
// the composer before the TUI has wired up input handling — a dimmed
// example prompt such as `Try "fix lint errors"`. The box is already
// drawn at this point, so it satisfies readyPattern exactly, which is
// why the naive signal reported ready roughly 100ms too early and
// keystrokes sent on that frame could be swallowed (🎯T284).
//
// It anchors on the composer body: the NBSP (U+00A0) Claude Code pads
// the prompt glyph with, then the literal `Try "`. Real input the owner
// has typed but not yet submitted still counts as ready — the one
// exception being a message that itself begins `Try "`, which costs a
// single extra poll and nothing else.
var startupPlaceholder = regexp.MustCompile(`^\x{00A0}?\s*Try "`)

// startupMenuCursor matches a selection menu's highlighted numbered
// option: the ❯ cursor immediately followed by a digit and a "." or
// ")" (e.g. "❯ 1. Resume from summary"). The idle input box also uses
// ❯ but never places a digit directly after it, so this discriminates
// a menu awaiting a choice from a ready prompt.
var startupMenuCursor = regexp.MustCompile(`❯\s*\d+[.)]`)

// resumePrompt matches Claude Code's specific stale-session resume
// wording as a belt-and-suspenders signal, in case the selection
// glyph renders differently across versions. This is the exact wedge
// T24 targets: a 5-day / 105k-token session parks the TUI at a
// "Resume from summary / Resume full session / Don't ask again" menu.
var resumePrompt = regexp.MustCompile(`(?i)resume (from summary|full session)|resume this session`)

// connectingPattern matches Claude Code's remote-control status while
// the TUI is still wiring. Paste/Enter during this window is swallowed
// or leaves a paste chip that never submits (🎯T305 live probe).
var connectingPattern = regexp.MustCompile(`(?i)/rc\s+connecting`)

// MatchConnecting reports whether the frame still shows Claude Code
// connecting (not yet accepting a durable turn).
func MatchConnecting(frame []byte) bool {
	return connectingPattern.Match(trimTrailingSpace(frame))
}

// MatchReady reports whether the captured frame shows Claude's idle
// input box at the tail of the visible pane AND that box is live —
// i.e. it will accept and submit a turn. The startup splash draws the
// same box holding a ghost placeholder while input is still dead, and
// is explicitly not ready. /rc connecting is also not ready (🎯T305).
func MatchReady(frame []byte) bool {
	if MatchConnecting(frame) {
		return false
	}
	body := composerBody(frame)
	return body != nil && !startupPlaceholder.Match(body)
}

// MatchStartupSplash reports whether the frame shows the composer box
// still holding Claude Code's ghost placeholder hint — drawn, but not
// yet accepting input. Exposed so callers probing launch behaviour can
// assert that a ready verdict never lands on a splash frame.
func MatchStartupSplash(frame []byte) bool {
	body := composerBody(frame)
	return body != nil && startupPlaceholder.Match(body)
}

// composerBody returns the text after the ❯ prompt glyph when the
// frame ends in the input box, or nil when there is no input box.
// A present-but-empty body is a non-nil empty slice.
func composerBody(frame []byte) []byte {
	m := readyPattern.FindSubmatch(trimTrailingSpace(frame))
	if m == nil {
		return nil
	}
	if m[1] == nil {
		return []byte{}
	}
	return m[1]
}

// MatchStartupMenu reports whether the captured frame shows a startup
// selection menu awaiting a keypress — most importantly Claude Code's
// resume/summary prompt for a stale session. When this is true and
// MatchReady is false, the launch handshake auto-confirms the
// highlighted default (Enter) rather than wedging until timeout.
func MatchStartupMenu(frame []byte) bool {
	f := trimTrailingSpace(frame)
	return startupMenuCursor.Match(f) || resumePrompt.Match(f)
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

const (
	// maxMenuDismissals bounds how many startup menus the launch
	// handshake will auto-confirm before giving up. A resume prompt may
	// be followed by another startup screen (e.g. a trust-folder
	// prompt), so we allow a few; if Enter never clears them we surface
	// a distinct error rather than pressing forever.
	maxMenuDismissals = 3
	// menuSettleDelay gives the TUI time to transition after an Enter
	// before the next capture, so we don't re-detect the same menu and
	// burn a dismissal on a frame that's mid-repaint.
	menuSettleDelay = 400 * time.Millisecond
)

// readyDriver abstracts the two side-effecting primitives WaitReady
// needs — capturing the pane and pressing Enter — so the poll loop can
// be exercised deterministically in tests without a live tmux server.
type readyDriver struct {
	capture   func() ([]byte, error)
	sendEnter func() error
}

// WaitReady polls capture-pane at `poll` intervals until MatchReady
// returns true or `timeout` elapses. Returns the elapsed time on
// success, or an error describing the last failure state on timeout.
//
// If a startup selection menu is detected (MatchStartupMenu) — chiefly
// Claude Code's stale-session resume/summary prompt — WaitReady
// auto-confirms the highlighted default by pressing Enter, up to
// maxMenuDismissals times, so a long-lived registered agent doesn't
// wedge at the menu (T24). If the menu never clears, the timeout error
// says so explicitly rather than emitting the generic
// "ready pattern did not match" message.
func WaitReady(windowID string, poll, timeout time.Duration) (time.Duration, error) {
	return waitReadyLoop(readyDriver{
		capture:   func() ([]byte, error) { return CapturePane(windowID) },
		sendEnter: func() error { return SendKeys(windowID, "") }, // empty msg → bare Enter (select default)
	}, poll, timeout, menuSettleDelay)
}

func waitReadyLoop(d readyDriver, poll, timeout, menuSettle time.Duration) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(timeout)

	var lastFrame []byte
	var lastErr error
	dismissals := 0
	menuSeen := false

	for {
		if !time.Now().Before(deadline) {
			return 0, readyTimeoutErr(menuSeen, dismissals, timeout, lastFrame, lastErr)
		}

		frame, err := d.capture()
		if err != nil {
			lastErr = err
			time.Sleep(poll)
			continue
		}
		lastFrame = frame

		if MatchReady(frame) {
			return time.Since(start), nil
		}

		if MatchStartupMenu(frame) && dismissals < maxMenuDismissals {
			menuSeen = true
			dismissals++
			if serr := d.sendEnter(); serr != nil {
				lastErr = serr
			}
			time.Sleep(menuSettle)
			continue
		}

		time.Sleep(poll)
	}
}

// readyTimeoutErr builds the timeout error, distinguishing a wedged
// startup menu (actionable) from a plain no-match or a capture that
// never succeeded.
func readyTimeoutErr(menuSeen bool, dismissals int, timeout time.Duration, lastFrame []byte, lastErr error) error {
	if menuSeen {
		return fmt.Errorf("startup menu (e.g. Claude Code's resume/summary prompt) still present after %d auto-confirmations within %s; last frame:\n%s", dismissals, timeout, lastFrame)
	}
	if lastFrame == nil {
		if lastErr != nil {
			return fmt.Errorf("capture-pane never succeeded within %s: %w", timeout, lastErr)
		}
		return fmt.Errorf("capture-pane never succeeded within %s", timeout)
	}
	return fmt.Errorf("ready pattern did not match within %s; last frame:\n%s", timeout, lastFrame)
}

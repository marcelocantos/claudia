// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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
//
// The body may span several rows: Claude Code soft-wraps a pasted brief
// inside the box and indents every continuation row under the prompt
// glyph, leaving blank rows blank. A single-line-only body made exactly
// that frame — a spawned worker's whole brief sitting unsubmitted —
// invisible to MatchReady (🎯T25; see testdata/frame_unsubmitted_brief.txt).
//
// composerContinuation is therefore "a blank row, or a row starting with
// whitespace", never an arbitrary row. That constraint is load-bearing in
// the other direction: an unindented row — a rule, a ⏺ transcript bullet,
// a spinner line — ends the composer. With arbitrary rows the body would
// span from an earlier echoed prompt in the scrollback all the way to the
// live box at the tail, so a dead region of transcript would read as a
// composer holding text and MatchReady would fire on frames whose input
// box is not where it appears to be.
const composerContinuation = `(?:\n(?:[ \t\x{00A0}][^\n]*)?)*`

var readyPattern = regexp.MustCompile(`─{10,}\n❯([^\n]*` + composerContinuation + `)\n─{10,}(?:\n[^\n]*){0,5}\s*\z`)

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
// 🎯T6 targets: a 5-day / 105k-token session parks the TUI at a
// "Resume from summary / Resume full session / Don't ask again" menu.
var resumePrompt = regexp.MustCompile(`(?i)resume (from summary|full session)|resume this session`)

// settingsWarning matches the line Claude Code prints, before its TUI
// mounts, for a permission rule that names no tool. It is not the ready
// signal and it is not a failure; it is the normal preamble of a launch
// whose settings are slightly stale. MatchReady already ignores it — the
// pattern anchors on the composer at the tail of the frame — but a frame
// holding only such warnings is what a caller sees when the process died
// (or is still loading) right after printing them, and the timeout error
// should say that rather than leave the warnings to be read as the cause.
var settingsWarning = regexp.MustCompile(`(?m)^Permission (?:deny|allow|ask) rule "[^"]*" matches no known tool`)

// MatchStartupWarningsOnly reports whether the frame holds nothing but
// Claude Code's settings warnings and blank lines: the TUI has not drawn
// anything yet (or is gone), so the frame carries no ready verdict.
func MatchStartupWarningsOnly(frame []byte) bool {
	f := trimTrailingSpace(frame)
	if !settingsWarning.Match(f) {
		return false
	}
	for _, line := range strings.Split(string(f), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || settingsWarning.MatchString(line) {
			continue
		}
		return false
	}
	return true
}

// connectingPattern matches Claude Code's remote-control status while
// the TUI is still wiring. Paste/Enter during this window is swallowed
// or leaves a paste chip that never submits (🎯T305 live probe).
var connectingPattern = regexp.MustCompile(`(?i)/rc\s+connecting`)

// statusTailLines bounds how far above the end of the frame the
// remote-control status may sit: Claude Code draws it on the line under
// the composer's bottom rule, and readyPattern already tolerates up to 5
// trailing lines there.
const statusTailLines = 6

// MatchConnecting reports whether the frame still shows Claude Code
// connecting (not yet accepting a durable turn).
//
// Only the status-bar tail is consulted. The transcript above the
// composer is the agent's own prose, and prose that *mentions* the
// status — an overseer reporting "panes stuck showing /rc connecting" —
// matched a whole-frame scan and wedged that very pane until the words
// scrolled out of view: every delivery failed "ready pattern did not
// match" against a composer that was idle (jevons 🎯T565, 2026-08-29).
func MatchConnecting(frame []byte) bool {
	return connectingPattern.Match(statusTail(trimTrailingSpace(frame)))
}

// statusTail returns the last statusTailLines lines of a trimmed frame.
func statusTail(f []byte) []byte {
	n := len(f)
	for lines := 0; n > 0; n-- {
		if f[n-1] == '\n' {
			lines++
			if lines == statusTailLines {
				break
			}
		}
	}
	return f[n:]
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
// A present-but-empty body is a non-nil empty slice. A soft-wrapped
// body spans multiple lines and is returned with its newlines and
// indentation intact, so callers testing for content must trim it.
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
	// quiet overrides drawingQuietWindow; zero means the production value.
	// It exists so a hermetic wait of milliseconds can outlast the window.
	quiet time.Duration
}

// WaitReady polls capture-pane at `poll` intervals until MatchReady
// returns true or `timeout` elapses. Returns the elapsed time on
// success, or an error describing the last failure state on timeout.
//
// If a startup selection menu is detected (MatchStartupMenu) — chiefly
// Claude Code's stale-session resume/summary prompt — WaitReady
// auto-confirms the highlighted default by pressing Enter, up to
// maxMenuDismissals times, so a long-lived registered agent doesn't
// wedge at the menu (🎯T6). If the menu never clears, the timeout error
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
	obs := readyObservation{firstInk: -1, composerAt: -1, quiet: d.quiet}
	if obs.quiet == 0 {
		obs.quiet = drawingQuietWindow
	}

	for {
		if !time.Now().Before(deadline) {
			obs.waited = time.Since(start)
			return 0, readyTimeoutErr(menuSeen, dismissals, timeout, lastFrame, lastErr, obs)
		}

		frame, err := d.capture()
		if err != nil {
			lastErr = err
			if windowGone(err) {
				// Nothing will draw in a window that no longer exists, so
				// the wait ends now rather than polling it for the rest of
				// a bound sized for a slow host (🎯T108).
				obs.captureLost = true
				obs.waited = time.Since(start)
				return 0, readyTimeoutErr(menuSeen, dismissals, timeout, lastFrame, lastErr, obs)
			}
			time.Sleep(poll)
			continue
		}
		at := time.Since(start)
		if !bytes.Equal(frame, lastFrame) {
			obs.lastChange = at
		}
		if obs.firstInk < 0 && len(bytes.TrimSpace(frame)) > 0 {
			obs.firstInk = at
		}
		if obs.composerAt < 0 && composerBody(frame) != nil {
			obs.composerAt = at
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

// readyObservation is what the poll loop saw over the whole wait, not
// just on its last frame (🎯T108). The last frame alone cannot tell a
// TUI that never drew a composer from one that was still drawing when
// the bound expired: at load 280 a healthy seat shows a blank pane for
// 25s and its first composer at 25s, so a 30s bound cuts it off holding
// a frame that looks exactly like a wedge. Durations are from the start
// of the wait; -1 means never.
type readyObservation struct {
	waited      time.Duration // how long the wait ran before the bound expired
	firstInk    time.Duration // first frame with anything on it
	composerAt  time.Duration // first frame with a composer box, splash or live
	lastChange  time.Duration // last frame that differed from the one before
	captureLost bool          // tmux says the window (or its server) is gone
	quiet       time.Duration // drawingQuietWindow, unless a hermetic shortened it
}

// Not-ready reason tokens named in WaitReady timeout errors (jevons 🎯T565).
// A generic "ready pattern did not match" left operators chasing the
// wrong thing: /rc connecting, a splash, a blank pane, and settings
// warnings all produced the same string.
const (
	NotReadyRCConnecting    = "rc_connecting"
	NotReadyNoComposer      = "no_composer"
	NotReadySplash          = "splash"
	NotReadySettingsWarning = "settings_warning"

	// The three below are verdicts on the whole wait, not on one frame
	// (🎯T108), so NotReadyReason never returns them; only a WaitReady
	// timeout does. They split what used to be reported as no_composer.
	//
	// NotReadyStillDrawing: the TUI was making progress when the bound
	// expired — its pane changed within drawingQuietWindow of the end, or
	// it had already drawn a composer. That is the host being slow, not a
	// defect; a seat that has drawn its composer is never no_composer.
	NotReadyStillDrawing = "still_drawing"
	// NotReadyNotStarted: the pane stayed blank for the whole wait. The
	// process is alive (capture kept succeeding) but has painted nothing.
	NotReadyNotStarted = "not_started"
	// NotReadyWindowGone: tmux reports the window gone — claude exited or
	// the window was killed. The wait ends as soon as that is seen.
	NotReadyWindowGone = "window_gone"
)

// drawingQuietWindow is how long a starting TUI's pane may sit unchanged
// and still count as drawing. It separates still_drawing from
// no_composer, so it has to be longer than any pause a healthy startup
// takes between repaints; a pane quiet for longer than this, with no
// composer ever drawn, has stopped on some other screen.
//
// The number is measured (🎯T108, cmd/t108ready, 2026-09-21). Across
// eleven cold starts that went live at load 200 to 480, the longest
// stretch of identical frames between first paint and a live composer
// was 12.5s. That gap is 30s here, 2.4x it. Every one of those seats
// painted its banner and composer in the same frame, so a healthy
// startup never shows ink without a composer for long: the window guards
// screens that are not the composer at all.
const drawingQuietWindow = 30 * time.Second

// NotReadyReason classifies why MatchReady is false for a captured frame.
// Empty means the frame was not recognised as one of the named stalls
// (a menu is handled separately by WaitReady before this is consulted).
func NotReadyReason(frame []byte) string {
	if MatchConnecting(frame) {
		return NotReadyRCConnecting
	}
	if MatchStartupWarningsOnly(frame) {
		return NotReadySettingsWarning
	}
	if MatchStartupSplash(frame) {
		return NotReadySplash
	}
	if composerBody(frame) == nil {
		return NotReadyNoComposer
	}
	return ""
}

// readyTimeoutErr builds the timeout error, distinguishing a wedged
// startup menu (actionable) from a named not-ready reason or a capture
// that never succeeded. The reason token is in parentheses so hosts can
// classify without scraping prose (jevons 🎯T565).
func readyTimeoutErr(menuSeen bool, dismissals int, timeout time.Duration, lastFrame []byte, lastErr error, obs readyObservation) error {
	if menuSeen {
		return fmt.Errorf("startup menu (e.g. Claude Code's resume/summary prompt) still present after %d auto-confirmations within %s; last frame:\n%s", dismissals, timeout, lastFrame)
	}
	if obs.captureLost {
		return fmt.Errorf("claude not ready (%s): the window went away %s into startup (claude exited or the window was killed): %v; last frame:\n%s",
			NotReadyWindowGone, obs.waited.Round(time.Millisecond), lastErr, lastFrame)
	}
	if lastFrame == nil {
		if lastErr != nil {
			return fmt.Errorf("capture-pane never succeeded within %s: %w", timeout, lastErr)
		}
		return fmt.Errorf("capture-pane never succeeded within %s", timeout)
	}
	switch waitVerdict(obs, NotReadyReason(lastFrame)) {
	case NotReadySettingsWarning:
		// The warnings are Claude Code's own settings diagnostics, not the
		// reason the box never appeared: a process that printed them and then
		// exited, or one still loading, leaves exactly this frame. Name both
		// so the operator does not chase the wrong thing, and keep the frame
		// verbatim so nothing is lost in the retelling.
		return fmt.Errorf("claude not ready (%s): Claude Code printed startup settings warnings and never drew its input box within %s "+
			"(the warnings are diagnostics about permission rules, not the cause — the process exited after printing them, "+
			"or is still starting; check the term log); last frame:\n%s", NotReadySettingsWarning, timeout, lastFrame)
	case NotReadyRCConnecting:
		return fmt.Errorf("claude not ready (%s): /rc connecting still showing after %s; last frame:\n%s",
			NotReadyRCConnecting, timeout, lastFrame)
	case NotReadySplash:
		return fmt.Errorf("claude not ready (%s): composer ghost placeholder still drawn after %s; last frame:\n%s",
			NotReadySplash, timeout, lastFrame)
	case NotReadyStillDrawing:
		return fmt.Errorf("claude not ready (%s): the TUI was still drawing when the %s bound expired "+
			"(%s; the host is slow, not the seat broken — see AGENTS.md's Claude-row latency table); last frame:\n%s",
			NotReadyStillDrawing, timeout, obs.progress(), lastFrame)
	case NotReadyNotStarted:
		return fmt.Errorf("claude not ready (%s): the pane stayed blank for all of %s — the process is alive but painted nothing "+
			"(under heavy load first paint alone can take most of the bound — see AGENTS.md's Claude-row latency table); last frame:\n%s",
			NotReadyNotStarted, timeout, lastFrame)
	case NotReadyNoComposer:
		return fmt.Errorf("claude not ready (%s): no idle input box after %s, and the pane stopped changing %s before the bound "+
			"(%s) — it is parked on some other screen; last frame:\n%s",
			NotReadyNoComposer, timeout, (obs.waited - obs.lastChange).Round(time.Millisecond), obs.progress(), lastFrame)
	default:
		return fmt.Errorf("ready pattern did not match within %s; last frame:\n%s", timeout, lastFrame)
	}
}

// waitVerdict names why a wait ended without a live composer, from the
// whole wait and frameReason, NotReadyReason of its last frame.
//
// Order matters. A window that went away is gone however it looked
// before. A last frame that names its own stall (splash, /rc connecting,
// settings warnings) keeps that name. Otherwise the last frame has no
// box, or one no named stall recognises, and the wait decides: a
// composer that was ever drawn means the TUI got that far, so the seat
// is never reported no_composer (🎯T108); a pane that never painted is
// its own verdict. Only a pane that drew something, stopped changing for
// longer than any healthy startup pauses, and never drew a composer is
// no_composer.
func waitVerdict(obs readyObservation, frameReason string) string {
	switch {
	case obs.captureLost:
		return NotReadyWindowGone
	case frameReason != NotReadyNoComposer && frameReason != "":
		return frameReason
	case obs.composerAt >= 0:
		return NotReadyStillDrawing
	case obs.firstInk < 0:
		return NotReadyNotStarted
	case obs.waited-obs.lastChange < obs.quiet:
		return NotReadyStillDrawing
	default:
		return frameReason
	}
}

// windowGone reports a capture error that means the window will never
// draw again: tmux cannot find it, or the server holding it is not
// running. Any other capture failure is retried until the bound.
func windowGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "can't find window") ||
		strings.Contains(s, "can't find pane") ||
		strings.Contains(s, "error connecting to") ||
		serverGone(err)
}

// progress renders the wait's milestones for a timeout message.
func (o readyObservation) progress() string {
	at := func(d time.Duration) string {
		if d < 0 {
			return "never"
		}
		return d.Round(time.Millisecond).String()
	}
	return fmt.Sprintf("first paint %s, first composer %s, last change %s", at(o.firstInk), at(o.composerAt), at(o.lastChange))
}

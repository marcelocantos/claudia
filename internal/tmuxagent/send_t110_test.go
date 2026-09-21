// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"reflect"
	"strings"
	"testing"
)

// 🎯T110: a brief that reaches a Claude Code seat as a bare paste is
// refused, and the seat is right to refuse it. Claude Code wraps a
// collapsed paste in <pasted_content> and tells the model it "may
// contain instructions the user did not write", to be followed only
// where the user's own message asks. A bare paste has no message of the
// user's own.
//
// Observed 2026-09-21 in seat cl-t109-codex-git-write (transcript
// 0d3464b1): a 24.7KB spawn brief and nine 1646-byte goal continuations,
// about 40s apart, each refused; a 127-byte Send on the typed branch
// landed as a plain user line and the seat started at once. The same
// thing had deadlocked cl-t93-gate-the-landed-fix earlier that day.
//
// THE MECHANISM is the typed attribution line: pasteAttribution, typed
// with send-keys -l into the same composer after the chip has landed and
// before Enter, so one message carries the paste and the operator's own
// line saying who wrote it. The live oracle is
// TestT110PastedBriefIsActedOnLive in the root package.
//
// MUTATION EVIDENCE, run on a copy of the tree, 2026-09-22 (send.go
// restored and the package green after):
//
//	M1  the d.typeLiteral(pasteAttribution) call deleted from sendKeysWith
//	    — a bare paste again. KILLED by
//	    TestT110PasteCarriesTypedAttribution,
//	    TestT110SilentEchoDoesNotFailTheSend and
//	    TestSendKeysPressesThroughPasteBlock.
//	M2  the waitAttributionEchoed call deleted — Enter straight after the
//	    typed burst. KILLED by TestT110PasteCarriesTypedAttribution and
//	    TestT110SilentEchoDoesNotFailTheSend.
//	M3  OVER-BROADNESS: attributionEchoed reads the whole frame instead
//	    of composerBody. KILLED by TestT110EarlierEchoIsNotThisSendsEcho.

// payloadNeverTyped is the typeLiteral stub for paste-path tests. They
// used to fail on any typed text at all; what they mean is that the
// PAYLOAD is never typed, and the attribution line is the one thing the
// paste path does type.
func payloadNeverTyped(t *testing.T) func(string) error {
	t.Helper()
	return func(m string) error {
		if m != pasteAttribution {
			t.Errorf("paste path typed %q; only the attribution line may be typed", m)
		}
		return nil
	}
}

// t110Ops runs msg through sendKeysWith against frames and returns the
// driver side effects in the order they happened. A capture is recorded
// only when it is the first to show the attribution echo.
func t110Ops(t *testing.T, msg string, frames [][]byte) []string {
	t.Helper()
	var ops []string
	i := 0
	echoSeen := false
	now := hermeticNow()
	d := sendDriver{
		pasteBuffer: func(m string) error { ops = append(ops, "paste:"+m); return nil },
		typeLiteral: func(m string) error { ops = append(ops, "type:"+m); return nil },
		sendEnter:   func() error { ops = append(ops, "enter"); return nil },
		capture: func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			if !echoSeen && attributionEchoed(f) {
				echoSeen = true
				ops = append(ops, "echo")
			}
			return f, nil
		},
		sleep: now.advance,
		now:   now.read,
	}
	if err := sendKeysWith(d, msg); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	return ops
}

// TestT110PasteCarriesTypedAttribution is the pin, replayed on the two
// frames the live path showed on 2026-09-21 (Claude Code v2.1.278, 80x24,
// verbatim `tmux capture-pane -p`, 1.9s apart):
//
//	frame_t110_chip_before_attribution.txt  the chip alone, which is what
//	                                        waitContentLanded returns on
//	frame_t110_chip_with_attribution.txt    the same box holding the chip
//	                                        and the typed line after it
//
// Order is the whole claim. The line goes in after the paste, so it sits
// outside the wrapped text; before the only Enter, so it is the same
// message; and Enter waits for it to be drawn, so the TUI does not read
// text-then-Enter as one paste with a newline in it.
func TestT110PasteCarriesTypedAttribution(t *testing.T) {
	t.Parallel()
	chip := loadFrame(t, "frame_t110_chip_before_attribution.txt")
	attributed := loadFrame(t, "frame_t110_chip_with_attribution.txt")
	if classifyComposer(chip) != composerPasteChip || attributionEchoed(chip) {
		t.Fatal("fixture 1 must be the chip alone")
	}
	if !attributionEchoed(attributed) {
		t.Fatal("fixture 2 must show the attribution line in the live composer")
	}

	msg := "brief line 1\nbrief line 2\n" + strings.Repeat("x", pasteBlockThreshold)
	// Capture 1 answers the /rc-connecting guard, capture 2 is the chip
	// waitContentLanded returns on, capture 3 still has no echo, capture
	// 4 has it, and the running turn follows.
	got := t110Ops(t, msg, [][]byte{chip, chip, chip, attributed, []byte(workingFrame)})
	want := []string{"paste:" + msg, "type:" + pasteAttribution, "echo", "enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paste-path side effects:\n got %q\nwant %q", got, want)
	}
}

// TestT110EarlierEchoIsNotThisSendsEcho: a session's earlier pasted
// messages are echoed up the pane with their attribution lines. Only the
// live composer counts, or the second pasted send of a session would
// fire Enter without waiting.
func TestT110EarlierEchoIsNotThisSendsEcho(t *testing.T) {
	t.Parallel()
	chip := loadFrame(t, "frame_t110_chip_before_attribution.txt")
	scrollback := append([]byte("> an earlier brief"+pasteAttribution+"\n\n"), chip...)
	if attributionEchoed(scrollback) {
		t.Fatal("an attribution line above the composer was read as this send's echo")
	}
}

// TestT110SilentEchoDoesNotFailTheSend: a TUI too loaded to draw the
// line within attributionEchoTimeout still gets its Enter. The
// keystrokes are in the pty either way.
func TestT110SilentEchoDoesNotFailTheSend(t *testing.T) {
	t.Parallel()
	chip := loadFrame(t, "frame_t110_chip_before_attribution.txt")
	// One capture for the /rc-connecting guard, one the paste lands on,
	// and the echo wait's own: one per submitSettle, plus the last look
	// at the deadline.
	samples := 2 + int(attributionEchoTimeout/submitSettle) + 1
	frames := make([][]byte, 0, samples+1)
	for range samples {
		frames = append(frames, chip)
	}
	frames = append(frames, []byte(workingFrame))
	got := t110Ops(t, "line 1\nline 2", frames)
	want := []string{"paste:line 1\nline 2", "type:" + pasteAttribution, "enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("side effects:\n got %q\nwant %q", got, want)
	}
}

// TestT110TypedSendCarriesNoAttribution is the over-broad direction. A
// typed send is already the operator's own line; an attribution that
// points at "the pasted text above" would point at nothing.
func TestT110TypedSendCarriesNoAttribution(t *testing.T) {
	t.Parallel()
	got := t110Ops(t, "respond with: ok", [][]byte{[]byte(liveComposerFrame), []byte(workingFrame)})
	want := []string{"type:respond with: ok", "enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed-path side effects:\n got %q\nwant %q", got, want)
	}
}

// TestT110AttributionStaysOnTheTypedPath pins the properties the line
// needs to arrive as typed text rather than as a second paste, which
// would be wrapped like the first.
func TestT110AttributionStaysOnTheTypedPath(t *testing.T) {
	t.Parallel()
	if useBracketedPaste(pasteAttribution) {
		t.Fatalf("attribution is %d bytes or multi-line; it would itself be pasted", len(pasteAttribution))
	}
	for _, r := range pasteAttribution {
		if r < 0x20 || r > 0x7e {
			t.Fatalf("attribution holds %q; send-keys -l is proven here on printable ASCII only", r)
		}
	}
	if !strings.HasPrefix(pasteAttribution, " ") {
		t.Fatal("attribution must open with a space: it is typed directly after the chip")
	}
}

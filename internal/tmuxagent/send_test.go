// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"strings"
	"testing"
	"time"
)

// pasteChipFrame mirrors a real capture-pane frame after a multi-KB
// brief lands as a collapsed Claude Code paste (term evidence for
// session 9184aff2 / 🎯T305 Failure B).
const pasteChipFrame = "" +
	"\n\n\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"❯ [Pasted text #1 +16 lines]\n" +
	"  paste again to expand\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

// workingFrame: composer gone / turn in progress (no paste chip).
const workingFrame = "" +
	"\n\n  thinking…\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"

func TestMatchUnsubmittedPaste(t *testing.T) {
	t.Parallel()
	if !MatchUnsubmittedPaste([]byte(pasteChipFrame)) {
		t.Fatal("paste chip frame must match")
	}
	if MatchUnsubmittedPaste([]byte(liveComposerFrame)) {
		t.Fatal("empty live composer must not match paste chip")
	}
	if MatchUnsubmittedPaste([]byte(workingFrame)) {
		t.Fatal("working frame must not match paste chip")
	}
	// Spaces optional (CSI-separated tokens in raw term dumps).
	if !MatchUnsubmittedPaste([]byte("[Pastedtext#2+17lines]")) {
		t.Fatal("compact Pastedtext#N form must match")
	}
}

func TestUseBracketedPaste(t *testing.T) {
	t.Parallel()
	if useBracketedPaste("hi") {
		t.Fatal("short single-line must use typeLiteral path")
	}
	if !useBracketedPaste("line1\nline2") {
		t.Fatal("any newline must use bracketed paste")
	}
	big := strings.Repeat("x", pasteBlockThreshold)
	if !useBracketedPaste(big) {
		t.Fatal("size at threshold must use bracketed paste")
	}
}

func TestClassifyComposer(t *testing.T) {
	t.Parallel()
	if got := classifyComposer([]byte(workingFrame)); got != composerWorking {
		t.Fatalf("working: %v", got)
	}
	if got := classifyComposer([]byte(pasteChipFrame)); got != composerPasteChip {
		t.Fatalf("paste: %v", got)
	}
	if got := classifyComposer([]byte(typedNotSubmittedFrame)); got != composerTyped {
		t.Fatalf("typed: %v", got)
	}
	if got := classifyComposer([]byte(liveComposerFrame)); got != composerEmptyIdle {
		t.Fatalf("empty idle: %v", got)
	}
}

// 🎯T305 (b): large / paste-chip path must re-press Enter until the turn
// begins (composer leaves idle), and error if it never does.
func TestSendKeysPressesThroughPasteBlock(t *testing.T) {
	t.Parallel()
	var enters int
	var typed, pasted string
	frames := []string{pasteChipFrame, pasteChipFrame, workingFrame}
	i := 0
	d := sendDriver{
		typeLiteral: func(msg string) error { typed = msg; return nil },
		pasteBuffer: func(msg string) error { pasted = msg; return nil },
		sendEnter:   func() error { enters++; return nil },
		capture: func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return []byte(f), nil
		},
		sleep: func(time.Duration) {},
	}
	// Multi-line forces pasteBuffer path.
	msg := "brief line 1\nbrief line 2\n" + strings.Repeat("x", 50)
	if err := sendKeysWith(d, msg); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	if typed != "" {
		t.Fatalf("short path used typeLiteral unexpectedly: %q", typed)
	}
	if pasted != msg {
		t.Fatalf("pasteBuffer got %q want full msg", pasted)
	}
	// First Enter after paste + at least one retry while chip visible.
	if enters < 2 {
		t.Fatalf("enters=%d want >=2 (initial + paste retry)", enters)
	}
}

// 🎯T305 (c): if paste never clears, Send must error (no silent success).
func TestSendKeysErrorsWhenPasteStuck(t *testing.T) {
	t.Parallel()
	d := sendDriver{
		typeLiteral: func(string) error { return nil },
		pasteBuffer: func(string) error { return nil },
		sendEnter:   func() error { return nil },
		capture:     func() ([]byte, error) { return []byte(pasteChipFrame), nil },
		sleep:       func(time.Duration) {},
	}
	err := sendKeysWith(d, "line1\nline2")
	if err == nil {
		t.Fatal("want error when paste chip never clears")
	}
	if !strings.Contains(err.Error(), "turn not submitted") {
		t.Fatalf("error should name unsubmitted turn: %v", err)
	}
}

// Empty idle after paste ⇒ brief never landed (Failure A class).
func TestSendKeysErrorsWhenComposerEmptyAfterPaste(t *testing.T) {
	t.Parallel()
	d := sendDriver{
		typeLiteral: func(string) error { return nil },
		pasteBuffer: func(string) error { return nil },
		sendEnter:   func() error { return nil },
		capture:     func() ([]byte, error) { return []byte(liveComposerFrame), nil },
		sleep:       func(time.Duration) {},
	}
	err := sendKeysWith(d, "line1\nline2")
	if err == nil {
		t.Fatal("want error when brief never reaches pane")
	}
	if !strings.Contains(err.Error(), "never reached pane") {
		t.Fatalf("error should name empty composer: %v", err)
	}
}

// Short single-line uses typeLiteral; confirm waits until working.
func TestSendKeysShortMessageLiteral(t *testing.T) {
	t.Parallel()
	var typed string
	var enters int
	d := sendDriver{
		typeLiteral: func(msg string) error { typed = msg; return nil },
		pasteBuffer: func(string) error {
			t.Fatal("pasteBuffer must not run for short single-line")
			return nil
		},
		sendEnter: func() error { enters++; return nil },
		// After Enter the pane leaves idle (turn began).
		capture: func() ([]byte, error) { return []byte(workingFrame), nil },
		sleep:   func(time.Duration) {},
	}
	if err := sendKeysWith(d, "hi"); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	if typed != "hi" {
		t.Fatalf("typed=%q", typed)
	}
	if enters != 1 {
		t.Fatalf("enters=%d want 1 (no paste retry)", enters)
	}
}

// Bare Enter (menu dismiss / empty msg) must not require capture confirm.
func TestSendKeysEmptyMsgBareEnter(t *testing.T) {
	t.Parallel()
	var enters int
	d := sendDriver{
		typeLiteral: func(string) error { t.Fatal("type"); return nil },
		pasteBuffer: func(string) error { t.Fatal("paste"); return nil },
		sendEnter:   func() error { enters++; return nil },
		capture: func() ([]byte, error) {
			t.Fatal("capture must not run for empty msg")
			return nil, nil
		},
		sleep: func(time.Duration) {},
	}
	if err := sendKeysWith(d, ""); err != nil {
		t.Fatal(err)
	}
	if enters != 1 {
		t.Fatalf("enters=%d", enters)
	}
}

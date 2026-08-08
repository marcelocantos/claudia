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

// connectingFrame: idle-looking box but still /rc connecting (not ready).
const connectingFrame = "" +
	"\n\n\n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"❯ \n" +
	"────────────────────────────────────────────────────────────────────────────────\n" +
	"  ⏵⏵ bypass permissions on (shift+tab to cycle) · /rc connecting…\n"

// hermeticDriver returns a sendDriver whose sleep advances a virtual clock.
func hermeticDriver(capture func() ([]byte, error), paste func(string) error, typeLit func(string) error) (sendDriver, *int) {
	enters := 0
	now := time.Now()
	return sendDriver{
		typeLiteral: typeLit,
		pasteBuffer: paste,
		sendEnter:   func() error { enters++; return nil },
		capture:     capture,
		sleep:       func(d time.Duration) { now = now.Add(d) },
		now:         func() time.Time { return now },
	}, &enters
}

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

func TestMatchConnecting(t *testing.T) {
	t.Parallel()
	if !MatchConnecting([]byte(connectingFrame)) {
		t.Fatal("connecting frame must match")
	}
	if MatchReady([]byte(connectingFrame)) {
		t.Fatal("connecting must not be ready")
	}
	if MatchConnecting([]byte(liveComposerFrame)) {
		t.Fatal("live composer must not match connecting")
	}
}

func TestMatchTurnInProgress(t *testing.T) {
	t.Parallel()
	if !MatchTurnInProgress([]byte(workingFrame)) {
		t.Fatal("thinking frame must match turn in progress")
	}
	if MatchTurnInProgress([]byte(pasteChipFrame)) {
		t.Fatal("paste chip is not turn in progress")
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
	// Working chrome wins over residual paste expand hint.
	mixed := workingFrame + "\npaste again to expand\n"
	if got := classifyComposer([]byte(mixed)); got != composerWorking {
		t.Fatalf("working+residual paste hint: %v", got)
	}
}

// 🎯T305 (b): large / paste-chip path must re-press Enter until the turn
// begins (composer leaves idle), and error if it never does.
func TestSendKeysPressesThroughPasteBlock(t *testing.T) {
	t.Parallel()
	var typed, pasted string
	frames := []string{pasteChipFrame, pasteChipFrame, pasteChipFrame, workingFrame}
	i := 0
	d, enters := hermeticDriver(
		func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return []byte(f), nil
		},
		func(msg string) error { pasted = msg; return nil },
		func(msg string) error { typed = msg; return nil },
	)
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
	if *enters < 2 {
		t.Fatalf("enters=%d want >=2 (initial + paste retry)", *enters)
	}
}

// 🎯T305 (c): if paste never clears, Send must error (no silent success).
func TestSendKeysErrorsWhenPasteStuck(t *testing.T) {
	t.Parallel()
	d, _ := hermeticDriver(
		func() ([]byte, error) { return []byte(pasteChipFrame), nil },
		func(string) error { return nil },
		func(string) error { return nil },
	)
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
	d, _ := hermeticDriver(
		func() ([]byte, error) { return []byte(liveComposerFrame), nil },
		func(string) error { return nil },
		func(string) error { return nil },
	)
	err := sendKeysWith(d, "line1\nline2")
	if err == nil {
		t.Fatal("want error when brief never reaches pane")
	}
	if !strings.Contains(err.Error(), "never appeared") && !strings.Contains(err.Error(), "never reached pane") {
		t.Fatalf("error should name empty/missing paste: %v", err)
	}
}

// Refuse paste while /rc connecting (live failure mode).
func TestSendKeysWaitsForConnectingThenPastes(t *testing.T) {
	t.Parallel()
	var pasted string
	frames := []string{connectingFrame, connectingFrame, pasteChipFrame, workingFrame}
	i := 0
	d, _ := hermeticDriver(
		func() ([]byte, error) {
			f := frames[min(i, len(frames)-1)]
			i++
			return []byte(f), nil
		},
		func(msg string) error { pasted = msg; return nil },
		func(string) error { t.Fatal("type"); return nil },
	)
	if err := sendKeysWith(d, "line1\nline2"); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	if pasted == "" {
		t.Fatal("paste never ran after connecting cleared")
	}
}

// Short single-line uses typeLiteral; confirm waits until working.
func TestSendKeysShortMessageLiteral(t *testing.T) {
	t.Parallel()
	var typed string
	d, enters := hermeticDriver(
		func() ([]byte, error) { return []byte(workingFrame), nil },
		func(string) error {
			t.Fatal("pasteBuffer must not run for short single-line")
			return nil
		},
		func(msg string) error { typed = msg; return nil },
	)
	if err := sendKeysWith(d, "hi"); err != nil {
		t.Fatalf("sendKeysWith: %v", err)
	}
	if typed != "hi" {
		t.Fatalf("typed=%q", typed)
	}
	if *enters != 1 {
		t.Fatalf("enters=%d want 1 (no paste retry)", *enters)
	}
}

// Bare Enter (menu dismiss / empty msg) must not require capture confirm.
func TestSendKeysEmptyMsgBareEnter(t *testing.T) {
	t.Parallel()
	// Empty msg still waits not-connecting, then bare Enter, no ensureSubmitted.
	d, enters := hermeticDriver(
		func() ([]byte, error) { return []byte(liveComposerFrame), nil },
		func(string) error { t.Fatal("paste"); return nil },
		func(string) error { t.Fatal("type"); return nil },
	)
	if err := sendKeysWith(d, ""); err != nil {
		t.Fatal(err)
	}
	if *enters != 1 {
		t.Fatalf("enters=%d", *enters)
	}
}

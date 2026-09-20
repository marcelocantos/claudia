// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// waitTranscriptSettled blocks until the transcript has stopped growing
// for a beat. A turn's last records can land after WaitForResponse has
// returned, and a window returned to the pool mid-write would hand its
// own tail to the next holder — which is the thing under test, so the
// test must not be the one creating it.
func waitTranscriptSettled(t *testing.T, path string) {
	t.Helper()
	var last int64 = -1
	stableSince := time.Now()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		size := int64(0)
		if fi, err := os.Stat(path); err == nil {
			size = fi.Size()
		}
		if size != last {
			last, stableSince = size, time.Now()
		} else if time.Since(stableSince) > 750*time.Millisecond {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("transcript %s never settled; continuing", path)
}

// poolTurn sends prompt to a pooled seat and returns the turn's text
// along with every Event the seat published while this holder had it.
func poolTurn(t *testing.T, ctx context.Context, a *Agent, prompt string) (string, []Event) {
	t.Helper()
	take := collectEvents(a)
	if err := a.Send(prompt); err != nil {
		t.Fatalf("Send(%q): %v", prompt, err)
	}
	text, err := a.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("WaitForResponse after %q: %v", prompt, err)
	}
	return text, take()
}

// TestPoolAgentEventsLive is 🎯T78's live gate on the direct pool: a
// pooled Claude seat discovers the transcript Claude writes, tails it,
// and answers WaitForResponse — and a window returned and acquired again
// gives its new holder a stream of that holder's own turn, with nothing
// replayed from the holder before it.
func TestPoolAgentEventsLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test starts a real Claude session)")
	}
	for _, bin := range []string{"claude", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	// jevonsd's pane census reaps windows on the shared claudia socket
	// that it does not recognise, so these seats get a server of their
	// own (🎯T77).
	isolateTmux(t)

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	first, err := AcquireDirect(ctx, Config{WorkDir: workDir, Model: "haiku"})
	if err != nil {
		t.Fatalf("AcquireDirect: %v", err)
	}
	window := first.WindowID()

	// The seat knows which transcript it is watching before it has said
	// anything — that identity is what the old pool lacked.
	if first.JSONLPath() == "" {
		t.Fatal("acquired seat has no transcript path")
	}
	sid, ok := tmuxagent.GetWindowOption(window, poolSessionOption)
	if !ok || strings.TrimSpace(sid) != first.SessionID() {
		t.Fatalf("@%s = %q (present=%v), want the seat's session %q",
			poolSessionOption, sid, ok, first.SessionID())
	}

	firstText, firstEvents := poolTurn(t, ctx, first, "Reply with exactly: PEAR")
	t.Logf("first holder: session %s answered %q over %d events",
		first.SessionID(), firstText, len(firstEvents))
	if !strings.Contains(firstText, "PEAR") {
		t.Fatalf("first turn answered %q, want it to contain PEAR", firstText)
	}
	if _, err := os.Stat(first.JSONLPath()); err != nil {
		t.Fatalf("stat transcript the seat claimed: %v", err)
	}

	waitTranscriptSettled(t, first.JSONLPath())
	if err := first.Release("return"); err != nil {
		t.Fatalf("Release(return): %v", err)
	}

	second, err := AcquireDirect(ctx, Config{WorkDir: workDir, Model: "haiku"})
	if err != nil {
		t.Fatalf("warm AcquireDirect: %v", err)
	}
	defer func() { _ = second.Release("drop") }()
	if second.WindowID() != window {
		t.Fatalf("warm acquire got window %s, want %s", second.WindowID(), window)
	}

	secondText, secondEvents := poolTurn(t, ctx, second, "Reply with exactly: PLUM")
	t.Logf("second holder: answered %q over %d events", secondText, len(secondEvents))
	if !strings.Contains(secondText, "PLUM") {
		t.Fatalf("second turn answered %q, want it to contain PLUM", secondText)
	}
	if strings.Contains(secondText, "PEAR") {
		t.Fatalf("the new holder's turn text carried the previous holder's answer: %q", secondText)
	}
	for _, ev := range secondEvents {
		if ev.Type == "assistant" && strings.Contains(ev.Text, "PEAR") {
			t.Fatalf("the previous holder's turn was replayed to the new holder: %q", ev.Text)
		}
	}
}

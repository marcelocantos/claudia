// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)

// TestAcquireLive is 🎯T64's and 🎯T78's live gate: through an in-process
// daemon on a private tmux server, a real Claude pool window is acquired,
// sent a prompt, answered, returned, and acquired warm again by a second
// consumer, which gets the same window back without a second spawn and is
// answered on its own turn — without the first consumer's turn arriving
// on it.
func TestAcquireLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test starts a real Claude session)")
	}
	for _, bin := range []string{"claude", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	d := startLiveDaemon(t)
	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	first, err := claudia.Acquire(ctx, claudia.Config{WorkDir: workDir, Model: "haiku"})
	if err != nil {
		t.Fatalf("Acquire via daemon: %v", err)
	}
	if !first.DaemonHeld() {
		t.Fatal("Acquire did not go through the daemon")
	}
	window := first.WindowID()
	if first.JSONLPath() == "" {
		t.Fatal("the daemon granted a seat with no transcript path")
	}
	firstText := poolAnswer(t, ctx, first, "Reply with exactly: PEAR")
	t.Logf("first consumer: seat %s answered %q", first.SessionID(), firstText)
	if !strings.Contains(firstText, "PEAR") {
		t.Fatalf("first turn answered %q, want it to contain PEAR", firstText)
	}
	if err := first.Release("return"); err != nil {
		t.Fatalf("Release(return): %v", err)
	}
	waitFor(t, "seat back in the pool", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.grants) == 0
	})

	start := time.Now()
	second, err := claudia.Acquire(ctx, claudia.Config{WorkDir: workDir, Model: "haiku"})
	if err != nil {
		t.Fatalf("warm Acquire via daemon: %v", err)
	}
	warm := time.Since(start)
	defer func() { _ = second.Release("drop") }()
	if second.WindowID() != window {
		t.Fatalf("second consumer got window %s, want warm %s", second.WindowID(), window)
	}
	t.Logf("warm re-acquire of %s through the daemon took %s", window, warm.Round(time.Millisecond))

	secondText := poolAnswer(t, ctx, second, "Reply with exactly: PLUM")
	t.Logf("second consumer: answered %q", secondText)
	if !strings.Contains(secondText, "PLUM") {
		t.Fatalf("second turn answered %q, want it to contain PLUM", secondText)
	}
	if strings.Contains(secondText, "PEAR") {
		t.Fatalf("the warm seat replayed the first consumer's turn: %q", secondText)
	}
}

// poolAnswer sends prompt to a daemon-held pooled seat and returns the
// turn's text, failing the test rather than hanging to the deadline.
func poolAnswer(t *testing.T, ctx context.Context, a *claudia.Agent, prompt string) string {
	t.Helper()
	if err := a.Send(prompt); err != nil {
		t.Fatalf("Send(%q) on the acquired seat: %v", prompt, err)
	}
	turn, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	text, err := a.WaitForResponse(turn)
	if err != nil {
		t.Fatalf("WaitForResponse after %q: %v", prompt, err)
	}
	return text
}

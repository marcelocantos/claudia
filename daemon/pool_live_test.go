// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)

// TestAcquireLive is 🎯T64's live gate: through an in-process daemon on a
// private tmux server, a real Claude pool window is acquired, sent a prompt,
// returned, and acquired warm again by a second consumer, which gets the
// same window back without a second spawn. (A pooled agent publishes no turn
// events yet, so the prompt's answer is not read back: 🎯T78.)
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
	if err := first.Send("Reply with exactly: ok"); err != nil {
		t.Fatalf("Send on the acquired seat: %v", err)
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
}

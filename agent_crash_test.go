// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// TestCrashSurvival used to prove a tmux window outlived its consumer.
// That window is gone. After the live gates, KillWindow refuses and the
// parent does not spawn the helper. The helper stays for the env pin.
func TestCrashSurvival(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test starts a real Claude Session)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	// The CLI window this test used to crash-survive is gone. KillWindow
	// refuses before tmux runs, so the parent does not spawn the helper.
	if err := tmuxagent.KillWindow("@1"); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("KillWindow = %v, want legacy refusal", err)
	}
}

// TestCrashSurvivalHelper is the child-process side of
// TestCrashSurvival. It is never run directly by `go test` — it
// only executes when CLAUDIA_CRASH_TEST_HELPER=1 is set. It starts
// an Agent, prints its window ID and session ID, then blocks forever
// on stdin (waiting to be killed).
func TestCrashSurvivalHelper(t *testing.T) {
	if os.Getenv("CLAUDIA_CRASH_TEST_HELPER") != "1" {
		t.Skip("helper — only runs as a subprocess of TestCrashSurvival")
	}

	workDir := os.Getenv("CLAUDIA_CRASH_TEST_WORKDIR")
	if workDir == "" {
		t.Fatal("CLAUDIA_CRASH_TEST_WORKDIR not set")
	}

	agent, err := Start(Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Intentionally no defer agent.Stop() — the parent kills us
	// with SIGKILL to simulate an uncontrolled crash.

	// Wait for readiness so the TUI is fully rendered before we
	// announce ourselves. Otherwise the parent kills us during
	// startup and capture-pane sees a blank viewport.
	//
	// 🎯T97 exemption: this runs only as the child process of the live
	// TestCrashSurvival, and WaitReady bounds a real Claude TUI coming up —
	// a genuinely unbounded wait on a real backend, not a fixture read.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	// Print the window ID and session ID so the parent can verify.
	fmt.Printf("WINDOW=%s\n", agent.tmuxWindowID)
	fmt.Printf("SESSION=%s\n", agent.sessionID)

	// Block until killed.
	select {}
}

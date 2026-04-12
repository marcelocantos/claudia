// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// TestCrashSurvival is the central property the tmux pivot rests on:
// if the consumer process dies mid-session, the Claude Code process
// keeps running inside its tmux window and can be adopted by a fresh
// consumer.
//
// Flow:
//  1. Parent spawns a child subprocess (re-invoking the test binary
//     with -test.run=TestCrashSurvivalHelper) which starts a
//     tmux-backed Agent, prints the window ID and session ID, then
//     blocks on stdin.
//  2. Parent reads the window ID and session ID from the child's
//     stdout.
//  3. Parent kills the child with SIGKILL — uncontrolled crash, no
//     defer or cleanup runs.
//  4. Parent verifies:
//     a. The tmux window is still alive.
//     b. capture-pane returns non-empty content (the TUI is still
//     rendered).
//     c. The session ID stored in the window's @claudia-session-id
//     user option matches what the child printed.
//  5. Parent kills the window to clean up.
//
// This test requires `claude` and `tmux` on PATH; skipped otherwise.
func TestCrashSurvival(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}

	workDir := t.TempDir()

	// Spawn a child process that starts an Agent and blocks.
	child := exec.Command(
		os.Args[0],
		"-test.run=^TestCrashSurvivalHelper$",
		"-test.v",
	)
	child.Env = append(os.Environ(),
		"CLAUDIA_CRASH_TEST_HELPER=1",
		"CLAUDIA_CRASH_TEST_WORKDIR="+workDir,
	)
	childStdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	child.Stderr = os.Stderr

	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	// Read the window ID and session ID the child prints.
	scanner := bufio.NewScanner(childStdout)

	readLine := func(prefix string) string {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatalf("timeout waiting for %q from child", prefix)
			default:
			}
			if !scanner.Scan() {
				t.Fatalf("child stdout closed before %q (err: %v)", prefix, scanner.Err())
			}
			line := scanner.Text()
			t.Logf("child: %s", line)
			if after, ok := strings.CutPrefix(line, prefix); ok {
				return after
			}
		}
	}

	windowID := readLine("WINDOW=")
	sessionID := readLine("SESSION=")

	t.Logf("child reported window=%s session=%s", windowID, sessionID)

	// Kill the child with SIGKILL — no cleanup runs.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = child.Wait() // reap; exit code is irrelevant

	t.Log("child killed with SIGKILL")

	// Give tmux a moment to notice the client detached (it shouldn't
	// affect the window, but be fair to the test environment).
	time.Sleep(200 * time.Millisecond)

	// Verify the window is still alive.
	if !tmuxagent.IsWindowAlive(windowID) {
		t.Fatal("tmux window died when consumer was killed — crash survival failed")
	}
	t.Log("window still alive after consumer death")

	// Verify capture-pane returns content (TUI is still rendered).
	frame, err := tmuxagent.CapturePane(windowID)
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	if len(strings.TrimSpace(string(frame))) == 0 {
		t.Fatal("capture-pane returned empty frame — TUI is gone")
	}
	t.Logf("capture-pane returned %d bytes", len(frame))

	// Verify the session ID is preserved in the window option.
	got, ok := tmuxagent.GetWindowOption(windowID, "claudia-session-id")
	if !ok {
		t.Fatal("@claudia-session-id not set on window after consumer crash")
	}
	if got != sessionID {
		t.Errorf("session ID mismatch: window has %q, child reported %q", got, sessionID)
	}
	t.Log("session ID preserved in tmux window option")

	// Clean up the window.
	if err := tmuxagent.KillWindow(windowID); err != nil {
		t.Errorf("cleanup kill-window: %v", err)
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

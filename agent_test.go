// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestEscapeWorkDir(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/Users/marcelo/work", "-Users-marcelo-work"},
		{"/Users/marcelo/work/github.com/marcelocantos/claudia",
			"-Users-marcelo-work-github-com-marcelocantos-claudia"},
		{"simple-dash", "simple-dash"},
		{"with_underscore", "with-underscore"},
		{"with spaces", "with-spaces"},
		{"with.dots", "with-dots"},
		{"Mixed123-CASE", "Mixed123-CASE"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := escapeWorkDir(tc.in); got != tc.want {
			t.Errorf("escapeWorkDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProjectDir(t *testing.T) {
	home := os.Getenv("HOME")
	got := projectDir("/Users/marcelo/work/claudia")
	want := filepath.Join(home, ".claude", "projects", "-Users-marcelo-work-claudia")
	if got != want {
		t.Errorf("projectDir = %q, want %q", got, want)
	}
}

func TestTermLogDirDefault(t *testing.T) {
	// With XDG_STATE_HOME unset, termLogDir falls back to ~/.local/state.
	t.Setenv("XDG_STATE_HOME", "")
	home := os.Getenv("HOME")
	got := termLogDir("/Users/marcelo/work/claudia")
	want := filepath.Join(home, ".local", "state", "claudia", "terms",
		"-Users-marcelo-work-claudia")
	if got != want {
		t.Errorf("termLogDir (default) = %q, want %q", got, want)
	}
}

func TestTermLogDirRespectsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/custom/state")
	got := termLogDir("/Users/marcelo/work/claudia")
	want := filepath.Join("/custom/state", "claudia", "terms",
		"-Users-marcelo-work-claudia")
	if got != want {
		t.Errorf("termLogDir (XDG) = %q, want %q", got, want)
	}
}

// TestAgentSmoke spawns a real claude process and exercises the
// readiness detector end-to-end. Gated on `claude` being available on
// PATH; skipped otherwise (CI without the binary installed, contributors
// without Claude Code). When it runs, it validates:
//
//   - Start succeeds and returns a live Agent
//   - WaitReady returns nil within the overall readiness cap
//   - The readiness latency is in a plausible range (>0, well under cap)
//   - CLAUDECODE stripping: the child doesn't detect itself as nested,
//     which we verify indirectly by reaching readiness at all. Before
//     the CLAUDECODE fix, a nested child would hang in startup and
//     WaitReady would timeout.
//
// The test does not send any prompts — that would incur API costs and
// make the test slow. Readiness detection is the surface being
// exercised here.
func TestAgentReadinessSmoke(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	if runtime.GOOS == "windows" {
		t.Skip("claudia Session mode does not support Windows")
	}

	workDir := t.TempDir()

	agent, err := Start(Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if !agent.Alive() {
		t.Fatal("Alive = false immediately after Start")
	}

	waitStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), readyOverallTimeout+5*time.Second)
	defer cancel()

	if err := agent.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	latency := time.Since(waitStart)
	if latency < 100*time.Millisecond {
		// Suspicious — the detector should need at least one poll cycle
		// plus the quiescence window.
		t.Errorf("WaitReady returned in %s — too fast, detector probably misfired", latency)
	}
	if latency > readyOverallTimeout {
		t.Errorf("WaitReady took %s, exceeds cap %s", latency, readyOverallTimeout)
	}
	t.Logf("WaitReady end-to-end: %s", latency.Round(time.Millisecond))
}

// TestAgentReadinessFailureOnDeadProcess verifies that if the Claude
// process dies during startup (before the TUI quiesces), detectReady
// sets a sensible error and closes the channel rather than hanging.
// We simulate a dead process by spawning claude with a flag that
// causes it to exit immediately — --help is a reasonable stand-in for
// "claude that exits without finishing startup".
func TestAgentReadinessFailureOnDeadProcess(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	workDir := t.TempDir()

	// ExtraArgs lets us inject --help so claude prints its usage and
	// exits cleanly, which from claudia's perspective looks like
	// "child exited during startup".
	agent, err := Start(Config{
		WorkDir:   workDir,
		ExtraArgs: []string{"--help"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// WaitReady should return within a reasonable time with a non-nil
	// error — either "claude exited before TUI became ready" or
	// possibly nil if the burst of --help output happened to quiesce
	// before the process exited. Either is acceptable; the hard
	// requirement is that WaitReady returns at all.
	done := make(chan error, 1)
	go func() { done <- agent.WaitReady(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("WaitReady returned error (expected): %v", err)
		} else {
			t.Log("WaitReady returned nil (--help burst quiesced before exit)")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("WaitReady hung beyond reasonable bound on a dead process")
	}
}

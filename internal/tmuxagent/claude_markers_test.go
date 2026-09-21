// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func TestStripClaudeSessionMarkersKeepsConfiguration(t *testing.T) {
	t.Parallel()
	env := []string{
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_SESSION_ID=abc",
		"CLAUDECODE=1",
		"CLAUDE_EFFORT=high",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"PATH=/bin",
	}
	got := StripClaudeSessionMarkers(env)
	want := []string{"CLAUDE_EFFORT=high", "CLAUDE_CODE_USE_BEDROCK=1", "PATH=/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSpawnedWindowCarriesNoClaudeSessionMarker is 🎯T121's leak oracle on
// the real path: a host running inside Claude Code starts the tmux server,
// and the seat's window must not inherit that session's markers — while the
// operator's Claude Code configuration still arrives.
func TestSpawnedWindowCarriesNoClaudeSessionMarker(t *testing.T) {
	sock := testServerSocket(t)
	for _, name := range ClaudeSessionMarkers {
		t.Setenv(name, "leaked-"+name)
	}
	t.Setenv("CLAUDE_EFFORT", "keep-me")

	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	out, err := exec.Command("tmux", "-S", sock, "show-environment", "-g").Output()
	if err != nil {
		t.Fatalf("show-environment: %v", err)
	}
	for _, name := range ClaudeSessionMarkers {
		if strings.Contains(string(out), "\n"+name+"=") || strings.HasPrefix(string(out), name+"=") {
			t.Errorf("%s is in the tmux server's global environment — every seat inherits it", name)
		}
	}

	workdir := t.TempDir()
	dump := filepath.Join(workdir, "env.txt")
	windowID, err := SpawnWindow(workdir, "markerprobe", "sh",
		[]string{"-c", "env > " + dump + ".tmp && mv " + dump + ".tmp " + dump + " && sleep 30"})
	if err != nil {
		t.Fatalf("SpawnWindow: %v", err)
	}
	t.Cleanup(func() { _ = KillWindow(windowID) })

	var data []byte
	backstop := wallclockguard.UntilTestTimeout(t)
	for {
		if data, err = os.ReadFile(dump); err == nil {
			break
		}
		if backstop.Err() != nil {
			t.Fatalf("window never wrote its environment to %s", dump)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		name, value, _ := strings.Cut(line, "=")
		// CLAUDECODE is set empty on purpose by SpawnWindow's -e.
		if slices.Contains(ClaudeSessionMarkers, name) && value != "" {
			t.Errorf("seat window inherited %s=%q", name, value)
		}
	}
	if !strings.Contains(string(data), "CLAUDE_EFFORT=keep-me") {
		t.Error("seat window lost CLAUDE_EFFORT — the strip is too wide and drops operator configuration")
	}
}

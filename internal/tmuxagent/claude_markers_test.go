// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

	dump := filepath.Join(t.TempDir(), "env.txt")
	id, spawnErr := SpawnWindow(t.TempDir(), "markerprobe", "sh", []string{"-c", "env > " + dump})
	if id != "" || !errors.Is(spawnErr, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, spawnErr)
	}
	if _, statErr := os.Stat(dump); !os.IsNotExist(statErr) {
		t.Fatalf("SpawnWindow wrote a window environment: %v", statErr)
	}
}

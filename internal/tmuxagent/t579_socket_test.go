// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTmux puts a recording `tmux` first on PATH. Every invocation is
// appended to a log file, one line of tab-separated argv per call. The
// script's behaviour is driven by `body`, a shell snippet run with the
// arguments in "$@" that must print the reply and set the exit status.
func fakeTmux(t *testing.T, body string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + logPath + "\n" +
		body + "\n"
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("no tmux calls recorded: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestT579SpawnWindowAlwaysPassesClaudiaSocket pins the socket on every
// tmux invocation the spawn path makes. A tmux call without -S talks to
// the user's default server (/private/tmp/tmux-<uid>/default on macOS),
// which is not where the fleet's panes live.
func TestT579SpawnWindowAlwaysPassesClaudiaSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	t.Setenv(tmuxSocketEnvVar, sock)

	logPath := fakeTmux(t, `
case "$3" in
  new-window) echo "@7" ;;
  show-environment) ;;
esac
exit 0`)

	id, err := SpawnWindow(t.TempDir(), "w", "true", nil)
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if raw, err := os.ReadFile(logPath); err == nil && strings.TrimSpace(string(raw)) != "" {
		t.Fatalf("tmux was invoked: %s", raw)
	}
}

// TestT579SpawnWindowRecoversWhenServerVanished: a vanished server is not
// restarted. SpawnWindow returns before tmux runs.
func TestT579SpawnWindowRecoversWhenServerVanished(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	t.Setenv(tmuxSocketEnvVar, sock)
	logPath := fakeTmux(t, `echo should-not-run; exit 1`)
	id, err := SpawnWindow(t.TempDir(), "w", "true", nil)
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if raw, readErr := os.ReadFile(logPath); readErr == nil && strings.TrimSpace(string(raw)) != "" {
		t.Fatalf("tmux was invoked: %s", raw)
	}
}

// TestT579EnsureServerRecreatesReapedAnchor: EnsureServer must not
// recreate an anchor session. It returns before tmux runs, so the
// socket file the server would bind does not appear.
func TestT579EnsureServerRecreatesReapedAnchor(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "t579")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	t.Setenv(tmuxSocketEnvVar, sock)
	if err := EnsureServer(); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("EnsureServer = %v, want legacy refusal", err)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("EnsureServer created a socket: %v", statErr)
	}
	id, err := SpawnWindow(t.TempDir(), "t579", "sleep", []string{"5"})
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
}

// TestT579SpawnWindowSurvivesKilledServer: neither EnsureServer nor
// SpawnWindow restarts a server. The socket file stays absent.
func TestT579SpawnWindowSurvivesKilledServer(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "t579")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	t.Setenv(tmuxSocketEnvVar, sock)
	if err := EnsureServer(); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("EnsureServer = %v, want legacy refusal", err)
	}
	id, err := SpawnWindow(t.TempDir(), "t579", "sleep", []string{"5"})
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("a refused spawn created a socket: %v", statErr)
	}
}

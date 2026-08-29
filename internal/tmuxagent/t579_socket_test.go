// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"os"
	"os/exec"
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
	if err != nil {
		t.Fatalf("SpawnWindow: %v", err)
	}
	if id != "@7" {
		t.Fatalf("window id = %q, want @7", id)
	}

	calls := readCalls(t, logPath)
	if len(calls) == 0 {
		t.Fatal("tmux was never invoked")
	}
	sawNewWindow := false
	for _, c := range calls {
		if !strings.HasPrefix(c, "-S "+sock+" ") {
			t.Errorf("tmux invoked without claudia's socket: %s", c)
		}
		if strings.Contains(c, " new-window ") {
			sawNewWindow = true
		}
	}
	if !sawNewWindow {
		t.Errorf("no new-window call recorded; calls: %v", calls)
	}
}

// TestT579SpawnWindowRecoversWhenServerVanished covers the live failure
// (🎯T579): EnsureServer proved a server, the server went away before
// new-window ran, and eight fleet-health recoveries reported
// "no server running on <sock>" as a spawn failure. The spawn must
// restart the server and succeed rather than surface that error.
func TestT579SpawnWindowRecoversWhenServerVanished(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	t.Setenv(tmuxSocketEnvVar, sock)
	marker := filepath.Join(t.TempDir(), "failed-once")

	logPath := fakeTmux(t, `
case "$3" in
  new-window)
    if [ ! -f `+marker+` ]; then
      : > `+marker+`
      echo "no server running on `+sock+`" >&2
      exit 1
    fi
    echo "@11"
    ;;
  show-environment) ;;
esac
exit 0`)

	id, err := SpawnWindow(t.TempDir(), "w", "true", nil)
	if err != nil {
		t.Fatalf("SpawnWindow did not recover from a vanished server: %v", err)
	}
	if id != "@11" {
		t.Fatalf("window id = %q, want @11", id)
	}

	newWindows := 0
	for _, c := range readCalls(t, logPath) {
		if strings.Contains(c, " new-window ") {
			newWindows++
		}
	}
	if newWindows != 2 {
		t.Fatalf("new-window attempts = %d, want 2 (fail then retry)", newWindows)
	}
}

// TestT579EnsureServerRecreatesReapedAnchor: agent windows are created
// inside the claudia-anchor session, so an anchor reaped by a pane
// census leaves new-window failing against a server that is up. Real
// tmux, throwaway socket.
func TestT579EnsureServerRecreatesReapedAnchor(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	sock := testServerSocket(t)
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	// Hold the server up with a second session, then reap the anchor.
	if out, err := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", "holder").CombinedOutput(); err != nil {
		t.Fatalf("holder session: %v: %s", err, out)
	}
	if out, err := exec.Command("tmux", "-S", sock, "kill-session", "-t", anchorSessionName).CombinedOutput(); err != nil {
		t.Fatalf("kill anchor: %v: %s", err, out)
	}

	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer with reaped anchor: %v", err)
	}
	if err := exec.Command("tmux", "-S", sock, "has-session", "-t", anchorSessionName).Run(); err != nil {
		t.Fatalf("anchor session not restored: %v", err)
	}
	if _, err := SpawnWindow(t.TempDir(), "t579", "sleep", []string{"5"}); err != nil {
		t.Fatalf("SpawnWindow after anchor recreate: %v", err)
	}
}

// TestT579SpawnWindowSurvivesKilledServer is the end-to-end shape of the
// production failure against real tmux: the server is killed after
// EnsureServer and the spawn still lands a window on claudia's socket.
func TestT579SpawnWindowSurvivesKilledServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	sock := testServerSocket(t)
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	_ = exec.Command("tmux", "-S", sock, "kill-server").Run()

	id, err := SpawnWindow(t.TempDir(), "t579", "sleep", []string{"5"})
	if err != nil {
		t.Fatalf("SpawnWindow after kill-server: %v", err)
	}
	if !strings.HasPrefix(id, "@") {
		t.Fatalf("window id = %q", id)
	}
	if !IsWindowAlive(id) {
		t.Fatalf("window %s not alive on claudia's socket", id)
	}
}

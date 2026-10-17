// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/testctlenv"
)

// testServerSocket points tmuxagent at a private tmux server for the
// duration of the test and tears it down afterwards. The socket lives
// in a short temp dir because AF_UNIX paths are capped near 104 bytes
// on macOS and t.TempDir()'s name is built from the test name.
func testServerSocket(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	dir, err := os.MkdirTemp("", "cltmux")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	sock := filepath.Join(dir, "s.sock")
	t.Setenv(tmuxSocketEnvVar, sock)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	return sock
}

// TestEnsureServerStripsTestControlEnv is the end-to-end oracle for
// target T20: it reproduces the original leak path and asserts it is
// closed.
//
// A tmux server freezes the environment of whichever process started
// it as its global environment, and every window created afterwards
// inherits that — not the environment of the client that ran
// new-window. So a test-control variable set when the server came up
// reaches every agent for the life of the server, which for a
// crash-survival helper means indefinitely.
//
// This test starts the server from a process environment carrying every
// test-control variable, then checks both the server's global
// environment and a real spawned window's environment.
func TestEnsureServerStripsTestControlEnv(t *testing.T) {
	sock := testServerSocket(t)

	for _, name := range testctlenv.All() {
		t.Setenv(name, "leaked-"+name)
	}
	// Configuration must survive the same trip; a strip that is too
	// wide breaks agents instead of protecting them.
	t.Setenv("CLAUDIA_CODEX_AUTH_PATH", "/keep/me")

	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	out, err := exec.Command("tmux", "-S", sock, "show-environment", "-g").Output()
	if err != nil {
		t.Fatalf("show-environment: %v", err)
	}
	globalEnv := string(out)
	for _, name := range testctlenv.All() {
		if strings.Contains(globalEnv, name+"=") {
			t.Errorf("%s is in the tmux server's global environment — every agent window inherits it", name)
		}
	}

	// The window is what actually matters: it is the shell a fleet
	// agent runs `go test ./...` in.
	dump := filepath.Join(t.TempDir(), "env.txt")
	id, err := SpawnWindow(t.TempDir(), "envprobe", "sh", []string{"-c", "env > " + dump})
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, statErr := os.Stat(dump); !os.IsNotExist(statErr) {
		t.Fatalf("SpawnWindow wrote a window environment: %v", statErr)
	}
}

// TestEnsureServerScrubsAlreadyRunningServer covers the healing path:
// a server that came up before this guard existed (or was started by
// anything else) still carries the stale variables, and claudia must
// clean it rather than leave it poisoned until the next reboot. This
// is the case that was live on the owner's machine — a server started
// weeks earlier by a crash-test helper.
func TestEnsureServerScrubsAlreadyRunningServer(t *testing.T) {
	sock := testServerSocket(t)

	// Bring a server up the old way: full environment, nothing stripped.
	start := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", anchorSessionName)
	start.Env = append(os.Environ(),
		"CLAUDIA_CRASH_TEST_HELPER=1",
		"CLAUDIA_CRASH_TEST_WORKDIR=/tmp/stale",
	)
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("seed server: %v: %s", err, out)
	}

	before, err := exec.Command("tmux", "-S", sock, "show-environment", "-g").Output()
	if err != nil {
		t.Fatalf("show-environment: %v", err)
	}
	if !strings.Contains(string(before), "CLAUDIA_CRASH_TEST_HELPER=1") {
		t.Skip("tmux did not capture the seeded variable; nothing to scrub")
	}

	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer on running server: %v", err)
	}

	after, err := exec.Command("tmux", "-S", sock, "show-environment", "-g").Output()
	if err != nil {
		t.Fatalf("show-environment: %v", err)
	}
	for _, name := range testctlenv.All() {
		if strings.Contains(string(after), name+"=") {
			t.Errorf("EnsureServer left %s in an already-running server's global environment", name)
		}
	}
}

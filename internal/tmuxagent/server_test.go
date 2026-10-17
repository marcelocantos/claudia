// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/testctlenv"
)

// TestEnsureServerStripsTestControlEnv: EnsureServer refuses before it
// can start a server, so a test-control variable in this process cannot
// be frozen into a tmux global environment. SpawnWindow refuses too.
func TestEnsureServerStripsTestControlEnv(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cltmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	t.Setenv(tmuxSocketEnvVar, sock)
	for _, name := range testctlenv.All() {
		t.Setenv(name, "leaked-"+name)
	}

	if err := EnsureServer(); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("EnsureServer = %v, want legacy refusal", err)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("EnsureServer created a socket: %v", statErr)
	}

	dump := filepath.Join(t.TempDir(), "env.txt")
	id, err := SpawnWindow(t.TempDir(), "envprobe", "sh", []string{"-c", "env > " + dump})
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, statErr := os.Stat(dump); !os.IsNotExist(statErr) {
		t.Fatalf("SpawnWindow wrote a window environment: %v", statErr)
	}
}

// TestEnsureServerScrubsAlreadyRunningServer keeps its name. EnsureServer
// returns before tmux, so it cannot start a server and cannot scrub one.
// A fake tmux on PATH would record a call.
func TestEnsureServerScrubsAlreadyRunningServer(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cltmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	t.Setenv(tmuxSocketEnvVar, sock)

	marker := filepath.Join(dir, "tmux-ran")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := EnsureServer(); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("EnsureServer = %v, want legacy refusal", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("EnsureServer executed tmux: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("EnsureServer created a socket: %v", statErr)
	}
}

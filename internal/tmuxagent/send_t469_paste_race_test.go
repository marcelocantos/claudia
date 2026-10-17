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

// TestT469SharedBufferNameRacesUnderBarrier used to race a shared paste
// buffer across minted panes. SpawnWindow refuses, so the race cannot run.
func TestT469SharedBufferNameRacesUnderBarrier(t *testing.T) {
	assertSpawnWindowRefuses(t)
}

// TestT469ConcurrentUniqueNamesUnderBarrier used to prove unique buffer
// names across minted panes. SpawnWindow refuses, so the race cannot run.
func TestT469ConcurrentUniqueNamesUnderBarrier(t *testing.T) {
	assertSpawnWindowRefuses(t)
}

// TestT469PasteViaBufferUsesUniqueNames used to drive pasteViaBuffer into
// minted panes. SpawnWindow refuses, so that path cannot run.
func TestT469PasteViaBufferUsesUniqueNames(t *testing.T) {
	assertSpawnWindowRefuses(t)
}

func assertSpawnWindowRefuses(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "bin")
	script := "#!/bin/sh\ntouch " + shellSingleQuote(marker) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	id, err := SpawnWindow(dir, "t469", bin, nil)
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("SpawnWindow executed the command: %v", statErr)
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestT469PasteFailureDeletesBuffer: EnsureServer refuses before a paste
// can load a buffer, and it does not create the socket that buffer would
// live on.
func TestT469PasteFailureDeletesBuffer(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "t469")
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
}

func isNoBufferErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no buffer") || strings.Contains(s, "buffer not found")
}

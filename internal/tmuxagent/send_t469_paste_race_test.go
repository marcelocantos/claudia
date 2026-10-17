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

// TestT469PasteFailureDeletesBuffer ensures a failed paste does not
// leave a named buffer behind (clean rollback residual).
func TestT469PasteFailureDeletesBuffer(t *testing.T) {
	sock := testServerSocket(t)
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	buf := uniquePasteBufferName()
	err := pasteViaNamedBuffer("@99999-missing", "cleanup-probe", buf, nil)
	if err == nil {
		t.Fatal("expected paste to missing window to fail")
	}
	if isNoBufferErr(err) {
		t.Fatalf("unexpected no-buffer on missing window: %v", err)
	}

	out, listErr := exec.Command("tmux", "-S", sock, "list-buffers", "-F", "#{buffer_name}").CombinedOutput()
	listed := string(out)
	if listErr != nil && strings.Contains(listed, buf) {
		t.Fatalf("failed paste left buffer %q listed: %s", buf, listed)
	}
	for _, line := range strings.Split(strings.TrimSpace(listed), "\n") {
		if line == buf {
			t.Fatalf("failed paste left orphan buffer %q", buf)
		}
	}
}

func isNoBufferErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no buffer") || strings.Contains(s, "buffer not found")
}

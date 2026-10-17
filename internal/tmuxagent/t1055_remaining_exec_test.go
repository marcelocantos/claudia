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

// TestT1055PasteAndPaneIDDoNotExec covers the unexported paste and pane
// helpers. A fake tmux on PATH would record a call, and CreateTemp would
// leave a file under TMPDIR. Neither may happen.
func TestT1055PasteAndPaneIDDoNotExec(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "t1055p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	t.Setenv("TMPDIR", sockDir)

	marker := filepath.Join(sockDir, "tmux-ran")
	binDir := filepath.Join(sockDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	hooked := false
	if err := pasteViaNamedBuffer("@1", "hello\nthere", "buf", func() { hooked = true }); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("pasteViaNamedBuffer = %v, want legacy refusal", err)
	}
	if hooked {
		t.Fatal("pasteViaNamedBuffer ran the load hook")
	}
	if err := pasteViaBuffer("@1", "hello"); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("pasteViaBuffer = %v, want legacy refusal", err)
	}
	if id, err := paneIDFor("@1"); id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("paneIDFor = (%q, %v), want legacy refusal", id, err)
	}

	entries, err := os.ReadDir(sockDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "claudia-send") {
			t.Fatalf("paste created %s", entry.Name())
		}
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("paste or paneID executed tmux: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("paste or paneID created a tmux socket: %v", statErr)
	}
}

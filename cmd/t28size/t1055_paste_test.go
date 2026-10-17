// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
)

// TestT1055ProbePasteDoesNotExec covers the probe's own paste and key
// helpers. A fake tmux on PATH would record a call, and CreateTemp would
// leave a file under TMPDIR. Neither may happen.
func TestT1055ProbePasteDoesNotExec(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "t1055z")
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

	if err := pasteBuffer("@1", "hello\nthere"); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("pasteBuffer = %v, want legacy refusal", err)
	}
	if err := sendKey("@1", "Enter"); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("sendKey = %v, want legacy refusal", err)
	}

	entries, err := os.ReadDir(sockDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "t28size-") {
			t.Fatalf("paste created %s", entry.Name())
		}
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("probe paste executed tmux: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("probe paste created a tmux socket: %v", statErr)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
)

// The command used to look up claude and spawn it in tmux. A resolvable
// claude and tmux must still produce the refusal, and neither binary runs.
func TestT108ReadyDoesNotSpawnCLI(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "marker")
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	for _, name := range []string{"claude", "tmux"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CLAUDE_BIN", filepath.Join(binDir, "claude"))
	t.Setenv("TMPDIR", binDir)

	err := run()
	if !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("run = %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("t108ready started a CLI: %v", statErr)
	}
}

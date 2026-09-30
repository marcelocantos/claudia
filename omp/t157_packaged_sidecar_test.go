// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"os"
	"path/filepath"
	"testing"
)

// 🎯T157: a released binary finds the sidecar its formula installed beside
// it, through Homebrew's opt/bin symlink; a binary with none beside it (a
// source checkout) finds nothing and falls back to the source tree.
func TestT157ReleasedBinaryFindsItsPackagedSidecar(t *testing.T) {
	root := t.TempDir()
	cellar := filepath.Join(root, "Cellar", "claudia", "0.45.0")
	sidecar := filepath.Join(cellar, "share", "claudia", "sidecar")
	for _, dir := range []string{filepath.Join(cellar, "bin"), sidecar, filepath.Join(root, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(cellar, "bin", "claudia")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "claudia")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}

	if got := packagedSidecarDir(link); got != "" {
		t.Fatalf("found %q with no server.ts installed", got)
	}
	if err := os.WriteFile(filepath.Join(sidecar, "server.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(sidecar)
	if got := packagedSidecarDir(link); got != want {
		t.Fatalf("through the symlink: %q, want %q", got, want)
	}
	if got := packagedSidecarDir(exe); got != want {
		t.Fatalf("direct: %q, want %q", got, want)
	}
}

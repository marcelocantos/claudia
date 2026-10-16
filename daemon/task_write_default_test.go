// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
)

// Colossus, 2026-10-13 (pimp-den-casks): a `broker grant --provider codex`
// seat came up read-only. The daemon gives an unrestricted codex grant the
// same workspace-write sandbox, with git write in a repo.
func TestGrantCodexGetsWriteSandbox(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := claudia.Start(claudia.Config{Name: "codex-seat", Provider: claudia.ProviderCodex, WorkDir: repo, SessionID: "sid-c", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start via daemon: %v", err)
	}
	t.Cleanup(a.Stop)
	def := f.d.reg.Def("codex-seat")
	if def == nil {
		t.Fatal("no registered def")
	}
	if def.SandboxMode != "workspace-write" {
		t.Fatalf("codex grant sandbox = %q, want workspace-write", def.SandboxMode)
	}
	if st := f.seat(0).start(); st.Config.SandboxMode != "workspace-write" {
		t.Fatalf("seat started with sandbox %q", st.Config.SandboxMode)
	}
}

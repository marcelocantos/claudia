// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
)

func TestLegacyGrantsMigrationRunsOnce(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "grants.json")
	r, err := claudia.NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	old := claudia.AgentDef{Name: "pimp-den-harness", Provider: claudia.ProviderGrok, AutoStart: true, SessionID: "legacy-session"}
	if err := r.Register(old); err != nil {
		t.Fatal(err)
	}
	if err := pruneObsoleteLegacyGrantsOnce(r, state); err != nil {
		t.Fatal(err)
	}
	if r.Def(old.Name) != nil {
		t.Fatal("old grant survived")
	}
	if _, err := os.Stat(filepath.Join(state, legacyGrantsMigrationMarker)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(old); err != nil {
		t.Fatal(err)
	}
	if err := pruneObsoleteLegacyGrantsOnce(r, state); err != nil {
		t.Fatal(err)
	}
	if r.Def(old.Name) == nil {
		t.Fatal("one-time migration removed a reused name")
	}
}

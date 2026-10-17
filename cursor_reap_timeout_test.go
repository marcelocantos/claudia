// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"testing"
)

// The store probe used to run lsof. A resolvable lsof and ps must still
// produce no pids, and neither binary runs.
func TestListStoreWritersLsofIsBounded(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.db")
	if err := os.WriteFile(store, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "marker")
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	for _, name := range []string{"lsof", "ps"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldCmd := lsofCommand
	lsofCommand = filepath.Join(dir, "lsof")
	t.Cleanup(func() { lsofCommand = oldCmd })
	t.Setenv("PATH", dir)
	if pids := listStoreWritersLsof(store); len(pids) != 0 {
		t.Fatalf("lsof probe reported pids: %v", pids)
	}
	if pids := listOrphanCursorACPPs(); len(pids) != 0 {
		t.Fatalf("ps probe reported pids: %v", pids)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("reap probe started a command: %v", statErr)
	}
}

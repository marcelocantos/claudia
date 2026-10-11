// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/marcelocantos/claudia"
)

const legacyGrantsMigrationMarker = "legacy-grants-t191.done"

// pruneObsoleteLegacyGrantsOnce runs before boot resume. The marker prevents
// deleting a new grant that happens to reuse an old name on later restarts.
// Save precedes marker: a crash between them safely retries the exact set.
func pruneObsoleteLegacyGrantsOnce(reg *claudia.Registry, stateDir string) error {
	marker := filepath.Join(stateDir, legacyGrantsMigrationMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	removed, err := reg.PruneObsoleteLegacyGrants()
	if err != nil {
		return err
	}
	// The list is deliberately logged for reconciliation against the scout.
	if len(removed) > 0 {
		fmt.Fprintf(os.Stderr, "pruned obsolete legacy grants: %v\n", removed)
	}
	return os.WriteFile(marker, []byte("T191 legacy grant inventory processed\n"), 0o600)
}

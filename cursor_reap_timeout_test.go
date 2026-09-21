// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An lsof that never answers must not hold the caller: the probe is bounded
// and reports nothing it could not establish.
func TestListStoreWritersLsofIsBounded(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.db")
	if err := os.WriteFile(store, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hang := filepath.Join(dir, "lsof")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldCmd, oldTimeout := lsofCommand, lsofTimeout
	lsofCommand, lsofTimeout = hang, 200*time.Millisecond
	defer func() { lsofCommand, lsofTimeout = oldCmd, oldTimeout }()
	start := time.Now()
	if pids := listStoreWritersLsof(store); len(pids) != 0 {
		t.Fatalf("a probe that timed out reported pids: %v", pids)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("store probe blocked for %s; it must be bounded", elapsed)
	}
}

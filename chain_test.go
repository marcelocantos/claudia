// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// setTestChainsDir redirects the chains directory to a temp dir for the
// duration of the test and restores the environment on cleanup.
func setTestChainsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Override XDG_STATE_HOME so chainsDir() returns a subpath of dir.
	t.Setenv("XDG_STATE_HOME", dir)
	return dir
}

func TestRegisterAndLookupChain(t *testing.T) {
	setTestChainsDir(t)

	chainID := "chain-abc"
	sessions := []string{"sess-1", "sess-2", "sess-3"}

	for _, sid := range sessions {
		if err := RegisterChain(chainID, sid); err != nil {
			t.Fatalf("RegisterChain(%q, %q): %v", chainID, sid, err)
		}
	}

	for _, sid := range sessions {
		cid, ids, err := LookupChain(sid)
		if err != nil {
			t.Fatalf("LookupChain(%q): %v", sid, err)
		}
		if cid != chainID {
			t.Errorf("LookupChain(%q) chainID = %q, want %q", sid, cid, chainID)
		}
		if len(ids) != len(sessions) {
			t.Errorf("LookupChain(%q) got %d ids, want %d", sid, len(ids), len(sessions))
		}
		for i, want := range sessions {
			if i < len(ids) && ids[i] != want {
				t.Errorf("LookupChain(%q) ids[%d] = %q, want %q", sid, i, ids[i], want)
			}
		}
	}
}

func TestLookupChainNotFound(t *testing.T) {
	setTestChainsDir(t)

	// Seed a different chain so the dir exists.
	if err := RegisterChain("other-chain", "other-sess"); err != nil {
		t.Fatal(err)
	}

	cid, ids, err := LookupChain("unknown-session")
	if err != nil {
		t.Fatalf("LookupChain: %v", err)
	}
	if cid != "" || ids != nil {
		t.Errorf("expected not-found, got cid=%q ids=%v", cid, ids)
	}
}

func TestLookupChainEmptyDir(t *testing.T) {
	setTestChainsDir(t)

	// Don't register anything — chains dir won't even exist.
	cid, ids, err := LookupChain("any-session")
	if err != nil {
		t.Fatalf("LookupChain on missing dir: %v", err)
	}
	if cid != "" || ids != nil {
		t.Errorf("expected not-found on empty dir, got cid=%q ids=%v", cid, ids)
	}
}

func TestConcurrentWriters(t *testing.T) {
	setTestChainsDir(t)

	chainID := "concurrent-chain"
	const numWriters = 20

	var wg sync.WaitGroup
	errs := make([]error, numWriters)
	for i := range numWriters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("sess-%02d", i)
			errs[i] = RegisterChain(chainID, sid)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}

	// Read the chain file directly to count lines.
	stateHome := os.Getenv("XDG_STATE_HOME")
	path := chainPath(stateHome+"/claudia/chains", chainID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chain file: %v", err)
	}

	var lines int
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != numWriters {
		t.Errorf("chain file has %d lines, want %d\ncontents:\n%s", lines, numWriters, data)
	}
}

func TestNewChainStartedByStart(t *testing.T) {
	// This test verifies that RegisterChain(id, id) correctly seeds a
	// single-entry chain — the pattern used by Start().
	setTestChainsDir(t)

	id := "initial-session-id"
	if err := RegisterChain(id, id); err != nil {
		t.Fatalf("RegisterChain: %v", err)
	}

	cid, ids, err := LookupChain(id)
	if err != nil {
		t.Fatalf("LookupChain: %v", err)
	}
	if cid != id {
		t.Errorf("chainID = %q, want %q", cid, id)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Errorf("ids = %v, want [%q]", ids, id)
	}
}

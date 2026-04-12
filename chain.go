// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// chainsDir returns the directory where chain sidecar files are stored.
// Follows XDG_STATE_HOME with a ~/.local/state fallback.
func chainsDir() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(stateHome, "claudia", "chains")
}

// chainPath returns the path to the .chain file for a given chainID.
func chainPath(dir, chainID string) string {
	return filepath.Join(dir, chainID+".chain")
}

// RegisterChain appends sessionID to the chain identified by chainID.
// If the chain file does not exist it is created. When chainID ==
// sessionID this effectively starts a new single-entry chain.
//
// Concurrent writes are serialised with an advisory exclusive flock so
// that multiple goroutines (or processes) can safely append to the same
// chain file.
func RegisterChain(chainID, sessionID string) error {
	dir := chainsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("chains dir: %w", err)
	}

	path := chainPath(dir, chainID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open chain file: %w", err)
	}
	defer f.Close()

	// Acquire exclusive advisory lock before appending.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	if _, err := fmt.Fprintln(f, sessionID); err != nil {
		return fmt.Errorf("write chain entry: %w", err)
	}
	return nil
}

// LookupChain searches all chain files in the chains directory for the
// one that contains sessionID. It returns the chainID and the full
// ordered list of session IDs in that chain. If no chain contains
// sessionID it returns ("", nil, nil).
func LookupChain(sessionID string) (chainID string, sessionIDs []string, err error) {
	dir := chainsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("read chains dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".chain") {
			continue
		}
		cid := strings.TrimSuffix(entry.Name(), ".chain")
		ids, found, err := scanChain(filepath.Join(dir, entry.Name()), sessionID)
		if err != nil {
			return "", nil, err
		}
		if found {
			return cid, ids, nil
		}
	}
	return "", nil, nil
}

// scanChain reads all session IDs from a chain file and reports
// whether target is among them. Reads are unsynchronised — chain files
// are append-only so a concurrent append either lands before or after
// our read cursor; either way the file is consistent.
func scanChain(path, target string) (ids []string, found bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open chain: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		ids = append(ids, line)
		if line == target {
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("scan chain: %w", err)
	}
	// Return the full list only when found.
	if !found {
		return nil, false, nil
	}
	return ids, true, nil
}

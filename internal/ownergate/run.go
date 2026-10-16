// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package ownergate runs the hermetic suite on an isolated committed snapshot.
package ownergate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/claudia/internal/testctlenv"
	"golang.org/x/sys/unix"
)

const Profile = "full-runtime-v1"

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func clean(dir string) error {
	status, err := git(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("uncommitted inputs in %s; commit or preserve them separately before make gate:\n%s", dir, status)
	}
	return nil
}

// Run refuses dirty input, serializes attestation writers, and measures a fresh
// checkout rather than the shared working directory. Ignored files, sibling Go
// workspaces and an existing node_modules cannot become hidden gate inputs.
func Run(dir string, output io.Writer) (err error) {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	gitDir, err := git(root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(gitDir, "gate.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Never unlink the lock: competing processes must lock the same inode.
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("owner gate busy or lock unavailable: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	attestation := filepath.Join(gitDir, "gate-attestation")
	if err := os.Remove(attestation); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := clean(root); err != nil {
		return err
	}
	commit, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	tree, err := git(root, "rev-parse", commit+"^{tree}")
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "claudia-owner-gate-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	snapshot := filepath.Join(tmp, "source")
	if _, err := git(root, "clone", "--shared", "--no-hardlinks", "--no-checkout", "--", root, snapshot); err != nil {
		return err
	}
	if _, err := git(snapshot, "checkout", "--detach", commit); err != nil {
		return err
	}
	fmt.Fprintf(output, "owner gate: commit=%s tree=%s profile=%s snapshot=%s\n", commit, tree, Profile, snapshot)
	cmd := exec.Command("make", "--no-print-directory", "gate-checks")
	cmd.Dir = snapshot
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Env = hermeticEnv()
	// A killed parent must not allow a second run to enter while make lives.
	cmd.ExtraFiles = []*os.File{lock}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("owner gate checks failed; nothing attested: %w", err)
	}
	for _, path := range []string{snapshot, root} {
		if err := clean(path); err != nil {
			return err
		}
		current, err := git(path, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return err
		}
		if current != tree {
			return fmt.Errorf("tree changed during owner gate in %s; nothing attested", path)
		}
	}
	// Rename a complete record, never expose a partially written attestation.
	// A cleanup failure is a failed run too: publish only after cleanup succeeds.
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	f, err := os.CreateTemp(gitDir, "gate-attestation-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := fmt.Fprintf(f, "profile=%s\ntree=%s\ncommit=%s\ndirty=0\nat=%s\n", Profile, tree, commit, time.Now().UTC().Format(time.RFC3339))
	err = errors.Join(writeErr, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), attestation); err != nil {
		return err
	}
	fmt.Fprintf(output, "✓ gate attestation: tree %s (%s)\n", tree, Profile)
	return nil
}

func hermeticEnv() []string {
	env := testctlenv.Strip(os.Environ())
	var result []string
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		// Git's repository-selection/config variables override -C and can make
		// snapshot checks accidentally inspect the caller's repository instead.
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		switch key {
		case "GOWORK", "GOFLAGS", "MAKEFLAGS", "MAKEFILES", "MFLAGS", "MAKELEVEL", "BUN_OPTIONS", "NODE_OPTIONS", "CLAUDIA_NO_BROKER":
			continue
		}
		result = append(result, entry)
	}
	return append(result, "GOWORK=off", "GOFLAGS=-mod=readonly", "CLAUDIA_NO_BROKER=1")
}

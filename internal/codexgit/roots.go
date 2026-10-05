// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package codexgit resolves Git metadata directories for Codex sandbox grants.
package codexgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const resolveTimeout = 30 * time.Second
const notARepoMarker = "not a git repository"

// WritableRoots returns the git directories a seat working in
// workDir writes when it commits or adds a worktree: the common dir
// (objects, refs, worktrees/) and, should it live elsewhere, the
// worktree's own git dir. Paths are absolute and symlink-free, which is
// the form the sandbox compares. A workDir outside any repository has
// nothing to grant and returns nil.
func WritableRoots(workDir string) ([]string, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		// No git for the host means no git for the seat it spawns with the
		// same PATH; there is no commit for the sandbox to block.
		slog.Warn("codex workspace-write: git not found, .git is left read-only", "workdir", workDir, "err", err)
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.Contains(stderr.String(), notARepoMarker) {
			return nil, nil
		}
		return nil, fmt.Errorf("git rev-parse in %s: %w: %s", workDir, err, strings.TrimSpace(stderr.String()))
	}

	var roots []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		dir := canonicalPath(strings.TrimSpace(line))
		if dir == "" || !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("git rev-parse in %s: unexpected git dir %q", workDir, line)
		}
		// The common dir comes first and normally contains the worktree's
		// own git dir (<common>/worktrees/<name>); one grant covers both.
		covered := false
		for _, r := range roots {
			if rel, rerr := filepath.Rel(r, dir); rerr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			roots = append(roots, dir)
		}
	}
	return roots, nil
}

// canonicalPath resolves symlinks when possible.
func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

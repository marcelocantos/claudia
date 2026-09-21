// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

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

// Codex's workspace-write sandbox makes the working directory writable
// and then carves `<root>/.git` back out as read-only (🎯T109). A seat in
// that sandbox can edit every file of its repo and cannot commit one:
//
//	fatal: Unable to create '<repo>/.git/index.lock': Operation not permitted
//	fatal: could not create directory of '.git/worktrees/tree': Operation not permitted
//
// A linked worktree fares no better — its `.git` is a file, and the
// directory that takes the writes is the shared one under the main
// checkout, outside the workdir altogether.
//
// No app-server field lifts the carve-out: thread/start's `sandbox` is a
// unit variant and there is no "protect .git" switch in the 0.155 schema.
// What does lift it is naming the git directory as a writable root of its
// own, which outranks the carve-out. Measured against codex-cli
// 0.155.0-alpha.9.2 on 2026-09-21 with command/exec (the sandbox, no
// model turn), repos outside /tmp so the default /tmp grant could not
// flatter the result:
//
//	workdir          writable_roots      git commit
//	main checkout    (none)              Operation not permitted
//	main checkout    <repo>/.git         ok
//	linked worktree  (none)              Operation not permitted
//	linked worktree  <main>/.git         ok
//
// So a workspace-write seat is granted its repo's git directories, and
// Start refuses when the app-server reports a sandbox without them.

// codexSandboxWorkspaceWrite is the thread/start sandbox mode that
// carries the `.git` carve-out.
const codexSandboxWorkspaceWrite = "workspace-write"

// codexGitResolveTimeout bounds the one `git rev-parse` Start runs. It is
// generous because Start already waits tens of seconds on a loaded host,
// and a timeout here refuses the seat.
const codexGitResolveTimeout = 30 * time.Second

// gitNotARepoMarker is what git says (under LC_ALL=C) when the directory
// is simply not in a repository — the one failure that means "nothing to
// grant" rather than "could not find out".
const gitNotARepoMarker = "not a git repository"

// codexGitWritableRoots returns the git directories a seat working in
// workDir writes when it commits or adds a worktree: the common dir
// (objects, refs, worktrees/) and, should it live elsewhere, the
// worktree's own git dir. Paths are absolute and symlink-free, which is
// the form the sandbox compares. A workDir outside any repository has
// nothing to grant and returns nil.
func codexGitWritableRoots(workDir string) ([]string, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		// No git for claudia means no git for the seat it spawns with the
		// same PATH; there is no commit for the sandbox to block.
		slog.Warn("codex workspace-write: git not found, .git is left read-only", "workdir", workDir, "err", err)
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexGitResolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.Contains(stderr.String(), gitNotARepoMarker) {
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

// canonicalPath is path with symlinks resolved where that is possible.
func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// checkGitRootsGranted is the refusal half of 🎯T109. The grant travels
// through CODEX_HOME/config.toml, a channel the app-server is free to
// ignore, so the sandbox it echoes is the only evidence the grant took.
// A workspace-write sandbox reported without a git root means the seat
// would come up able to edit and unable to commit; Start says so instead.
func checkGitRootsGranted(gitRoots []string, effective codexEffectiveSandbox) error {
	if effective.Type != codexSandboxTypeFor(codexSandboxWorkspaceWrite) {
		return nil // not the sandbox that carves .git out, or nothing reported
	}
	for _, root := range gitRoots {
		granted := false
		for _, e := range effective.WritableRoots {
			if canonicalPath(e) == root {
				granted = true
				break
			}
		}
		if !granted {
			return fmt.Errorf("codex workspace-write sandbox keeps %s read-only: the app-server did not take it as a writable root (effective writable roots %q), so git commit and git worktree add would fail with \"Operation not permitted\" — refusing to start a seat that cannot write its own .git",
				root, effective.WritableRoots)
		}
	}
	return nil
}

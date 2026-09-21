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
	"slices"
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
// So a workspace-write seat that sets Config.SandboxGitWrite is granted
// its repo's git directories, and Start refuses when the app-server
// reports a sandbox without them. The grant is opt-in (🎯T112): .git/hooks
// and .git/config run outside the sandbox on the operator's next git
// command, which is why Codex protects the directory in the first place.
// A seat that does not opt in starts with .git read-only and a log line
// saying so.
//
// The grant rides the app-server's argv (`-c`), not CODEX_HOME/config.toml
// where the caller's own roots go (🎯T598). config.toml needs a private
// home, and giving one to a seat that never had it would strand the
// threads it keeps in ~/.codex the next time it resumed. thread/start
// echoes a `-c` root exactly as it echoes a config.toml one (measured,
// same binary, a home with no config at all).

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

// codexSandboxConfigKey is the config path of the workspace-write
// writable roots, as `codex app-server -c <key>=<toml>` takes it.
const codexSandboxConfigKey = "sandbox_workspace_write.writable_roots"

// codexSandboxArgs are the app-server arguments that carry the git grant.
// A `-c` value replaces the config.toml list rather than adding to it, so
// the caller's roots are repeated here or the grant would cost them.
func codexSandboxArgs(t codexSandboxTuning) []string {
	if len(t.GitRoots) == 0 {
		return nil
	}
	roots := codexWritableRootsTOML(slices.Concat(t.WritableRoots, t.GitRoots))
	if roots == "" {
		return nil
	}
	return []string{"-c", codexSandboxConfigKey + "=" + roots}
}

// codexSandboxTuningFor is the sandbox a Start request asks for beyond
// its mode. workspace-write keeps the repo's .git read-only unless the
// git directories are granted as writable roots of their own (🎯T109),
// and that grant is the spawner's to ask for (🎯T112).
func codexSandboxTuningFor(req agentStartRequest) (codexSandboxTuning, error) {
	tuning := codexSandboxTuning{
		WritableRoots: req.Config.SandboxWritableRoots,
		NetworkAccess: req.Config.SandboxNetworkAccess,
	}
	if resolveCodexSandbox(req.Config.SandboxMode) != codexSandboxWorkspaceWrite {
		return tuning, nil
	}
	gitRoots, err := codexGitWritableRoots(req.WorkDir)
	switch {
	case req.Config.SandboxGitWrite && err != nil:
		return tuning, fmt.Errorf("codex: SandboxGitWrite asked for a writable .git and the git directory to grant could not be resolved — refusing to start a seat that cannot commit: %w", err)
	case req.Config.SandboxGitWrite:
		tuning.GitRoots = gitRoots
	default:
		noteCodexGitReadOnly(req.WorkDir, gitRoots, err)
	}
	return tuning, nil
}

// noteCodexGitReadOnly is the default arm of 🎯T112: a workspace-write
// seat that did not ask for SandboxGitWrite keeps Codex's protection of
// .git, and says so. The seat still starts — most seats never commit —
// but the first "Operation not permitted" from git has an explanation in
// the log above it, which is what 🎯T109 was filed for the lack of.
func noteCodexGitReadOnly(workDir string, gitRoots []string, resolveErr error) {
	switch {
	case resolveErr != nil:
		slog.Warn("codex workspace-write: .git stays read-only (SandboxGitWrite unset); could not tell whether the workdir is a repository",
			"workdir", workDir, "err", resolveErr)
	case len(gitRoots) > 0:
		slog.Warn("codex workspace-write: .git stays read-only, so this seat cannot git commit or git worktree add; set Config.SandboxGitWrite to grant it",
			"workdir", workDir, "read_only", gitRoots)
	}
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
// as a config override, which the app-server is free to ignore, so the
// sandbox it echoes is the only evidence the grant took.
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

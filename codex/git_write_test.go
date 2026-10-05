// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"bytes"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func gitWriteRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func TestStandaloneGitWriteArgvAndWarning(t *testing.T) {
	repo := gitWriteRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "--detach", linked)
	// An unborn repo cannot add a worktree; seed an empty commit.
	seed := exec.Command("git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "seed")
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v: %s", err, out)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v: %s", err, out)
	}
	for _, workDir := range []string{repo, linked} {
		t.Run(filepath.Base(workDir), func(t *testing.T) {
			root := filepath.Join(repo, ".git")
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			for _, enabled := range []bool{false, true} {
				task := NewTask(Config{WorkDir: workDir, SandboxMode: "workspace-write", SandboxGitWrite: enabled})
				roots, err := task.gitWriteRoots()
				if err != nil {
					t.Fatal(err)
				}
				got := execArgs(execArgInput{WorkDir: workDir, SandboxMode: task.sandbox, SandboxGitWrite: enabled, GitRoots: roots, Prompt: "p"})
				want := []string{"--cd", workDir, "--sandbox", "workspace-write"}
				if enabled {
					want = append(want, "-c", "sandbox_workspace_write.writable_roots=["+strconv.Quote(root)+"]")
				}
				want = append(want, "exec", "--json", "p")
				if !slices.Equal(got, want) {
					t.Fatalf("enabled=%v argv=%q want=%q", enabled, got, want)
				}
			}
			if !strings.Contains(logs.String(), ".git stays read-only") || !strings.Contains(logs.String(), "SandboxGitWrite") {
				t.Fatalf("no read-only warning: %s", logs.String())
			}
		})
	}
}

// Mutant oracle: dropping the opt-in or the grant must break argv equality.
func TestStandaloneGitWriteMutantOracle(t *testing.T) {
	root := "/main/.git"
	in := execArgInput{SandboxMode: "workspace-write", SandboxGitWrite: true, GitRoots: []string{root}, Prompt: "p"}
	baseline := execArgs(in)
	in.SandboxGitWrite = false
	if slices.Equal(baseline, execArgs(in)) {
		t.Fatal("disabled grant has same argv")
	}
	in.SandboxGitWrite = true
	in.GitRoots = nil
	if slices.Equal(baseline, execArgs(in)) {
		t.Fatal("lost roots have same argv")
	}
}

func TestStandaloneGitWriteUnresolvableRefuses(t *testing.T) {
	task := NewTask(Config{WorkDir: filepath.Join(t.TempDir(), "absent"), SandboxMode: "workspace-write", SandboxGitWrite: true})
	if _, err := task.gitWriteRoots(); err == nil {
		t.Fatal("missing workdir silently accepted")
	}
}

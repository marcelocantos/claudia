// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 🎯T116: `codex exec --sandbox workspace-write` keeps .git read-only just
// as the app-server does, and exits 0 with the commit refused. Task mode
// takes the same opt-in as Session mode (🎯T112): TaskConfig.SandboxGitWrite
// passes the git dir as a `-c` writable root; unset, a warning names the
// restriction. These tests pin the argv for both arms.

// t116Args is what RunTask hands to codex for req.
func t116Args(t *testing.T, req taskRunRequest) []string {
	t.Helper()
	granted, err := codexTaskGitGrant(req)
	if err != nil {
		t.Fatalf("codexTaskGitGrant: %v", err)
	}
	return codexTaskArgs(granted)
}

func TestT116GrantArmPutsTheGitDirBeforeExec(t *testing.T) {
	repo := t109Repo(t)
	got := t116Args(t, taskRunRequest{
		WorkDir:         repo,
		SandboxMode:     "workspace-write",
		SandboxGitWrite: true,
		ApprovalPolicy:  "never",
		Prompt:          "p",
	})
	want := []string{
		"--ask-for-approval", "never",
		"--cd", repo,
		"--sandbox", "workspace-write",
		"-c", "sandbox_workspace_write.writable_roots=[" + strconv.Quote(filepath.Join(repo, ".git")) + "]",
		"exec", "--json", "p",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant   %q", got, want)
	}
}

// From a linked worktree the root is the main checkout's .git, which is
// outside the workdir altogether.
func TestT116GrantArmFromALinkedWorktree(t *testing.T) {
	repo := t109Repo(t)
	linked := filepath.Join(canonicalPath(t.TempDir()), "linked")
	t109Git(t, repo, "worktree", "add", "-q", linked)
	got := t116Args(t, taskRunRequest{WorkDir: linked, SandboxMode: "workspace-write", SandboxGitWrite: true, Prompt: "p"})
	want := "sandbox_workspace_write.writable_roots=[" + strconv.Quote(filepath.Join(repo, ".git")) + "]"
	if !slices.Contains(got, want) {
		t.Fatalf("argv = %q, want it to carry %s", got, want)
	}
}

// The default arm: no grant on the argv, and a warning that names the
// directory and the field — `codex exec` itself exits 0 over the refusal.
func TestT116DefaultArmWithholdsTheGrantAndSaysSo(t *testing.T) {
	repo := t109Repo(t)
	logs := t112CaptureLogs(t)
	got := t116Args(t, taskRunRequest{WorkDir: repo, SandboxMode: "workspace-write", ApprovalPolicy: "never", Prompt: "p"})
	want := []string{"--ask-for-approval", "never", "--cd", repo, "--sandbox", "workspace-write", "exec", "--json", "p"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant   %q", got, want)
	}
	for _, mention := range []string{".git stays read-only", filepath.Join(repo, ".git"), "SandboxGitWrite"} {
		if !strings.Contains(logs.String(), mention) {
			t.Fatalf("log does not mention %q — a read-only .git must not be silent:\n%s", mention, logs)
		}
	}
}

// Modes that carve nothing out get no override and no warning.
func TestT116OtherModesAreLeftAlone(t *testing.T) {
	repo := t109Repo(t)
	logs := t112CaptureLogs(t)
	for _, mode := range []string{"", "read-only", "danger-full-access"} {
		got := t116Args(t, taskRunRequest{WorkDir: repo, SandboxMode: mode, SandboxGitWrite: mode == "danger-full-access", Prompt: "p"})
		if slices.Contains(got, "-c") {
			t.Fatalf("SandboxMode %q: argv = %q carries an override", mode, got)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("logged about a .git no sandbox is protecting:\n%s", logs)
	}
}

// The grant on a run whose mode claudia cannot vouch for would be dropped
// without a word; an empty SandboxMode leaves the mode to config.toml.
func TestT116GitWriteNeedsWorkspaceWrite(t *testing.T) {
	for _, mode := range []string{"", "read-only"} {
		err := codexTaskPrecheck(taskRunRequest{SandboxMode: mode, SandboxGitWrite: true})
		if err == nil {
			t.Fatalf("SandboxMode %q with SandboxGitWrite was accepted", mode)
		}
		for _, want := range []string{"SandboxGitWrite", "workspace-write"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal does not name %q: %v", want, err)
			}
		}
	}
	for _, mode := range []string{"workspace-write", "danger-full-access"} {
		if err := codexTaskPrecheck(taskRunRequest{SandboxMode: mode, SandboxGitWrite: true}); err != nil {
			t.Fatalf("SandboxMode %q with SandboxGitWrite refused: %v", mode, err)
		}
	}
}

// A task that asked for the grant where git cannot answer is refused by
// name; one that did not ask still runs.
func TestT116UnresolvableGitDirRefusesOnlyWhenAsked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	t112CaptureLogs(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	_, err := codexTaskGitGrant(taskRunRequest{WorkDir: missing, SandboxMode: "workspace-write", SandboxGitWrite: true})
	if err == nil || !strings.Contains(err.Error(), "SandboxGitWrite") {
		t.Fatalf("err = %v, want a refusal naming SandboxGitWrite", err)
	}
	if _, err := codexTaskGitGrant(taskRunRequest{WorkDir: missing, SandboxMode: "workspace-write"}); err != nil {
		t.Fatalf("a task that did not ask was refused: %v", err)
	}
}

// The field reaches the request a Run hands its backend, and crosses the
// broker wire.
func TestT116FieldReachesTheRunAndTheWire(t *testing.T) {
	cfg := TaskConfig{Provider: ProviderCodex, SandboxMode: "workspace-write", SandboxGitWrite: true}
	backend := &fakeTaskBackend{events: []TaskEvent{{Type: TaskEventResult, Content: "done"}}}
	events, err := newTaskWithBackend(cfg, backend).Run(t.Context(), "commit it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	if req := backend.request(t); !req.SandboxGitWrite || req.SandboxMode != "workspace-write" {
		t.Fatalf("Run handed the backend %+v: SandboxGitWrite did not arrive", req)
	}
	raw, err := EncodeTaskConfigWire(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sandbox_git_write":true`) {
		t.Fatalf("task wire lacks sandbox_git_write: %s", raw)
	}
	back, err := DecodeTaskConfigWire(raw)
	if err != nil || !back.SandboxGitWrite {
		t.Fatalf("round trip lost the field: %+v, %v", back, err)
	}
}

// Live: a workspace-write Task with the opt-in commits in its own repo. The
// outcome is read from the repository — `codex exec` exits 0 either way.
func TestCodexTaskGitWriteLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCodexBin(); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}
	repo := t109Repo(t)
	t109Git(t, repo, "config", "user.name", "t116")
	t109Git(t, repo, "config", "user.email", "t116@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "note.txt"), []byte("t116\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := NewTask(TaskConfig{
		ID:              "codex-git-write-smoke",
		Name:            "codex-git-write",
		Provider:        ProviderCodex,
		WorkDir:         repo,
		SandboxMode:     "workspace-write",
		SandboxGitWrite: true,
		ApprovalPolicy:  "never",
	})
	const subject = "t116-live-commit"
	events, err := task.Run(t.Context(), "Run exactly this shell command in the current directory and then reply with its output, nothing else:\n"+
		"git add note.txt && git commit -q -m "+subject+" && echo T116-OK")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for ev := range events {
		if ev.Type == TaskEventError {
			t.Fatalf("TaskEventError: %s", ev.ErrorMsg)
		}
	}

	log, err := exec.Command("git", "-C", repo, "log", "--format=%s", "-1").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := strings.TrimSpace(string(log)); got != subject {
		t.Fatalf("HEAD subject = %q, want %q — the task could not commit", got, subject)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 🎯T116: `codex exec --sandbox workspace-write` keeps .git read-only just
// as the app-server does, and exits 0 with the commit refused. Task mode
// takes the same opt-in as Session mode (🎯T112): TaskConfig.SandboxGitWrite
// passes the git dir as a `-c` writable root; unset, a warning names the
// restriction. These tests pin the argv for both arms.

// t116Args is what RunTask hands to codex for req.
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

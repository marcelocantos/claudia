// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveCodexTaskRun drives one real `codex exec --json` turn on the
// ChatGPT subscription. Gated on CLAUDIA_CODEX_LIVE=1 or CLAUDIA_LIVE=1.
func TestLiveCodexTaskRun(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" && os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE/CLAUDIA_LIVE not set (subscription / network)")
	}
	if _, err := resolveBin(nil); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}
	if err := ensureSubscriptionAuth(nil); err != nil {
		t.Skipf("subscription auth not ready: %v", err)
	}

	workDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	task := NewCodexTask(Config{
		ID:             "codex-pkg-live",
		Name:           "codex-pkg-live",
		WorkDir:        workDir,
		SandboxMode:    "read-only",
		ApprovalPolicy: "never",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	events, err := task.Run(ctx, "Reply with exactly: T14.2-ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawResult bool
	for ev := range events {
		switch ev.Type {
		case EventResult:
			sawResult = true
			if ev.Content == "" {
				t.Error("empty result content")
			}
		case EventError:
			t.Fatalf("EventError: %v", ev.Error)
		}
	}
	if !sawResult {
		t.Error("never saw EventResult")
	}
	if task.SessionID() == "" {
		t.Error("SessionID empty after live run")
	}
}

// TestLiveCodexStandaloneGitWriteCommit checks the actual repository state:
// codex exec may exit 0 even when its attempted git commit was denied.
func TestLiveCodexStandaloneGitWriteCommit(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (spends subscription credit)")
	}
	if _, err := resolveBin(nil); err != nil {
		t.Skipf("codex binary: %v", err)
	}
	if err := ensureSubscriptionAuth(nil); err != nil {
		t.Skipf("codex auth: %v", err)
	}
	repo := gitWriteRepo(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("config", "user.name", "codex-smoke")
	git("config", "user.email", "codex-smoke@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "note.txt"), []byte("standalone git write\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := NewTask(Config{WorkDir: repo, SandboxMode: "workspace-write", SandboxGitWrite: true, ApprovalPolicy: "never"})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	events, err := task.Run(ctx, "Run this exact shell command in the current directory: git add note.txt && git commit -q -m standalone-git-write-smoke && echo COMMITTED. Then report its output.")
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Type == EventError {
			t.Fatalf("codex error: %v", ev.Error)
		}
	}
	if got := git("log", "-1", "--format=%s"); got != "standalone-git-write-smoke" {
		t.Fatalf("HEAD subject=%q, commit did not land", got)
	}
}

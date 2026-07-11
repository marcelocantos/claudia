// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakeCLI writes an executable that prints fixturePath to stdout and
// exits with exitCode. It ignores argv so production arg construction does
// not need to match a real CLI. Used to exercise production RunTask spawn
// without network or a real coding agent.
func writeFakeCLI(t *testing.T, fixturePath string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hermetic fake CLI spawn tests use a POSIX shell script")
	}
	absFixture, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("Abs(%s): %v", fixturePath, err)
	}
	if _, err := os.Stat(absFixture); err != nil {
		t.Fatalf("fixture %s: %v", absFixture, err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-agent-cli")
	// #!/bin/sh — portable on macOS CI (bash 3.2) and Linux.
	script := "#!/bin/sh\n" +
		"cat \"" + absFixture + "\"\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake CLI: %v", err)
	}
	return bin
}

func drainTaskEvents(t *testing.T, events <-chan TaskEvent) []TaskEvent {
	t.Helper()
	var got []TaskEvent
	for ev := range events {
		got = append(got, ev)
	}
	return got
}

func TestHermeticTaskRunClaudeSpawn(t *testing.T) {
	bin := writeFakeCLI(t, "testdata/claude/exec/success.jsonl", 0)
	t.Setenv("CLAUDE_BIN", bin)

	task := NewTask(TaskConfig{
		ID:       "hermetic-claude",
		Provider: ProviderClaude,
		WorkDir:  t.TempDir(),
		Model:    "sonnet",
	})
	if _, ok := task.backend.(claudeTaskBackend); !ok {
		t.Fatalf("backend = %T, want claudeTaskBackend", task.backend)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := task.Run(ctx, "summarize")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drainTaskEvents(t, events)

	var sawInit, sawText, sawResult bool
	for _, ev := range got {
		switch ev.Type {
		case TaskEventInit:
			sawInit = true
			if ev.SessionID != "claude-sess-hermetic-1" {
				t.Errorf("SessionID = %q", ev.SessionID)
			}
		case TaskEventText:
			sawText = true
		case TaskEventResult:
			sawResult = true
			if ev.Content != "Final answer." {
				t.Errorf("Result = %q", ev.Content)
			}
		case TaskEventError:
			t.Errorf("unexpected error event: %s", ev.ErrorMsg)
		}
	}
	if !sawInit || !sawText || !sawResult {
		t.Fatalf("events incomplete: init=%v text=%v result=%v got=%#v", sawInit, sawText, sawResult, got)
	}
	if task.ClaudeID() != "claude-sess-hermetic-1" {
		t.Errorf("ClaudeID = %q", task.ClaudeID())
	}
	if task.LastResult() != "Final answer." {
		t.Errorf("LastResult = %q", task.LastResult())
	}
	if task.Status() != TaskStatusIdle {
		t.Errorf("Status = %q, want idle", task.Status())
	}
}

func TestHermeticTaskRunClaudeErrorEvent(t *testing.T) {
	bin := writeFakeCLI(t, "testdata/claude/exec/error.jsonl", 0)
	t.Setenv("CLAUDE_BIN", bin)

	task := NewTask(TaskConfig{
		ID:       "hermetic-claude-err",
		Provider: ProviderClaude,
		WorkDir:  t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := task.Run(ctx, "fail please")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drainTaskEvents(t, events)
	var sawErr bool
	for _, ev := range got {
		if ev.Type == TaskEventError && ev.IsError && strings.Contains(ev.ErrorMsg, "overloaded") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("expected TaskEventError with overloaded, got %#v", got)
	}
	if !strings.Contains(task.LastResult(), "overloaded") {
		t.Errorf("LastResult = %q", task.LastResult())
	}
}

func TestHermeticTaskRunCodexSpawn(t *testing.T) {
	bin := writeFakeCLI(t, "testdata/codex/exec/success.jsonl", 0)
	t.Setenv("CODEX_BIN", bin)

	task := NewTask(TaskConfig{
		ID:       "hermetic-codex",
		Provider: ProviderCodex,
		WorkDir:  t.TempDir(),
		Model:    "gpt-5.4",
	})
	if _, ok := task.backend.(codexTaskBackend); !ok {
		t.Fatalf("backend = %T, want codexTaskBackend", task.backend)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := task.Run(ctx, "summarize")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drainTaskEvents(t, events)

	var sawInit, sawResult bool
	for _, ev := range got {
		switch ev.Type {
		case TaskEventInit:
			sawInit = true
			if ev.SessionID == "" {
				t.Error("empty session id")
			}
		case TaskEventResult:
			sawResult = true
			if ev.Content != "Final answer." {
				t.Errorf("Result = %q", ev.Content)
			}
		}
	}
	if !sawInit || !sawResult {
		t.Fatalf("events incomplete: init=%v result=%v got=%#v", sawInit, sawResult, got)
	}
	if task.ClaudeID() == "" {
		t.Error("ClaudeID empty after Codex spawn")
	}
	if task.LastResult() != "Final answer." {
		t.Errorf("LastResult = %q", task.LastResult())
	}
}

func TestHermeticTaskRunGrokSpawn(t *testing.T) {
	bin := writeFakeCLI(t, "testdata/grok/exec/success.jsonl", 0)
	t.Setenv("GROK_BIN", bin)

	task := NewTask(TaskConfig{
		ID:       "hermetic-grok",
		Provider: ProviderGrok,
		WorkDir:  t.TempDir(),
		Model:    "grok-4",
	})
	if _, ok := task.backend.(grokTaskBackend); !ok {
		t.Fatalf("backend = %T, want grokTaskBackend", task.backend)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := task.Run(ctx, "summarize")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drainTaskEvents(t, events)

	var sawInit, sawResult bool
	for _, ev := range got {
		switch ev.Type {
		case TaskEventInit:
			sawInit = true
			if ev.SessionID != "019f4f39-0ad7-74c1-942b-32557bd3c302" {
				t.Errorf("SessionID = %q", ev.SessionID)
			}
		case TaskEventResult:
			sawResult = true
			if ev.Content != "Final answer." {
				t.Errorf("Result = %q", ev.Content)
			}
		}
	}
	if !sawInit || !sawResult {
		t.Fatalf("events incomplete: init=%v result=%v got=%#v", sawInit, sawResult, got)
	}
	if task.ClaudeID() != "019f4f39-0ad7-74c1-942b-32557bd3c302" {
		t.Errorf("ClaudeID = %q", task.ClaudeID())
	}
	if task.LastResult() != "Final answer." {
		t.Errorf("LastResult = %q", task.LastResult())
	}
}

func TestHermeticTaskRunNonExecutableBinary(t *testing.T) {
	// Point resolvers at a directory so Stat succeeds (env override is
	// honoured) but exec fails — hermetic without depending on a clean PATH
	// or absent host installs.
	notExec := t.TempDir()
	t.Setenv("CLAUDE_BIN", notExec)
	t.Setenv("CODEX_BIN", notExec)
	t.Setenv("GROK_BIN", notExec)

	for _, tc := range []struct {
		name     string
		provider Provider
	}{
		{"claude", ProviderClaude},
		{"codex", ProviderCodex},
		{"grok", ProviderGrok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := NewTask(TaskConfig{
				ID:       "badbin-" + tc.name,
				Provider: tc.provider,
				WorkDir:  t.TempDir(),
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := task.Run(ctx, "hi")
			if err == nil {
				t.Fatal("Run returned nil error for non-executable binary path")
			}
			if task.Status() != TaskStatusError {
				t.Errorf("Status = %q, want error", task.Status())
			}
		})
	}
}

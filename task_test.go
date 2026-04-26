// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTaskSystemEvent(t *testing.T) {
	line := `{"type":"system","subtype":"init","session_id":"abc-123","tools":[],"model":"claude-sonnet-4-6"}`
	events := ParseTaskLine([]byte(line))

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != TaskEventInit {
		t.Errorf("expected type %q, got %q", TaskEventInit, ev.Type)
	}
	if ev.SessionID != "abc-123" {
		t.Errorf("expected session_id %q, got %q", "abc-123", ev.SessionID)
	}
}

func TestParseTaskAssistantTextEvent(t *testing.T) {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "Hello, world!"},
			},
		},
	}
	line, _ := json.Marshal(msg)
	events := ParseTaskLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != TaskEventText {
		t.Errorf("expected type %q, got %q", TaskEventText, ev.Type)
	}
	if ev.Content != "Hello, world!" {
		t.Errorf("expected content %q, got %q", "Hello, world!", ev.Content)
	}
}

func TestParseTaskAssistantToolUseEvent(t *testing.T) {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_abc",
					"name":  "Bash",
					"input": map[string]any{"command": "ls -la"},
				},
			},
		},
	}
	line, _ := json.Marshal(msg)
	events := ParseTaskLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != TaskEventToolUse {
		t.Errorf("expected type %q, got %q", TaskEventToolUse, ev.Type)
	}
	if ev.ToolName != "Bash" {
		t.Errorf("expected tool name %q, got %q", "Bash", ev.ToolName)
	}
	if ev.ToolID != "toolu_abc" {
		t.Errorf("expected tool ID %q, got %q", "toolu_abc", ev.ToolID)
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(ev.ToolInput), &input); err != nil {
		t.Fatalf("failed to parse tool input: %v", err)
	}
	if input["command"] != "ls -la" {
		t.Errorf("expected command %q, got %q", "ls -la", input["command"])
	}
}

func TestParseTaskAssistantMixedContent(t *testing.T) {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "Let me check."},
				map[string]any{
					"type":  "tool_use",
					"id":    "toolu_xyz",
					"name":  "Read",
					"input": map[string]any{"file_path": "/tmp/test"},
				},
			},
		},
	}
	line, _ := json.Marshal(msg)
	events := ParseTaskLine(line)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != TaskEventText {
		t.Errorf("event 0: expected type %q, got %q", TaskEventText, events[0].Type)
	}
	if events[1].Type != TaskEventToolUse {
		t.Errorf("event 1: expected type %q, got %q", TaskEventToolUse, events[1].Type)
	}
}

func TestParseTaskResultSuccess(t *testing.T) {
	msg := map[string]any{
		"type":           "result",
		"subtype":        "success",
		"result":         "Done!",
		"duration_ms":    1234.5,
		"total_cost_usd": 0.05,
		"usage": map[string]any{
			"input_tokens":                4,
			"output_tokens":               12,
			"cache_creation_input_tokens": 12582,
			"cache_read_input_tokens":     4802,
		},
	}
	line, _ := json.Marshal(msg)
	events := ParseTaskLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != TaskEventResult {
		t.Errorf("expected type %q, got %q", TaskEventResult, ev.Type)
	}
	if ev.Content != "Done!" {
		t.Errorf("expected content %q, got %q", "Done!", ev.Content)
	}
	if ev.DurationMs != 1234.5 {
		t.Errorf("expected duration 1234.5, got %f", ev.DurationMs)
	}
	if ev.CostUSD != 0.05 {
		t.Errorf("expected cost 0.05, got %f", ev.CostUSD)
	}
	if ev.IsError {
		t.Error("expected IsError false")
	}
	if ev.Usage.InputTokens != 4 {
		t.Errorf("expected input_tokens 4, got %d", ev.Usage.InputTokens)
	}
}

func TestParseTaskResultError(t *testing.T) {
	msg := map[string]any{
		"type":    "result",
		"subtype": "error_tool",
		"errors": []any{
			map[string]any{"message": "tool failed"},
			map[string]any{"message": "another error"},
		},
	}
	line, _ := json.Marshal(msg)
	events := ParseTaskLine(line)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != TaskEventError {
		t.Errorf("expected type %q, got %q", TaskEventError, ev.Type)
	}
	if !ev.IsError {
		t.Error("expected IsError true")
	}
	if ev.ErrorMsg != "tool failed; another error" {
		t.Errorf("expected error msg %q, got %q", "tool failed; another error", ev.ErrorMsg)
	}
}

func TestParseTaskUnknownType(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_delta"}}`
	events := ParseTaskLine([]byte(line))
	if len(events) != 0 {
		t.Errorf("expected 0 events for unknown type, got %d", len(events))
	}
}

func TestParseTaskInvalidJSON(t *testing.T) {
	events := ParseTaskLine([]byte("not json"))
	if len(events) != 0 {
		t.Errorf("expected 0 events for invalid JSON, got %d", len(events))
	}
}

func TestParseTaskEmptyContent(t *testing.T) {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": ""},
			},
		},
	}
	line, _ := json.Marshal(msg)
	events := ParseTaskLine(line)
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty text, got %d", len(events))
	}
}

func TestNewTask(t *testing.T) {
	s := NewTask(TaskConfig{
		ID:      "test-123",
		Name:    "my task",
		WorkDir: "/tmp",
		Model:   "sonnet",
	})
	if s.TaskID() != "test-123" {
		t.Errorf("expected ID %q, got %q", "test-123", s.TaskID())
	}
	if s.TaskName() != "my task" {
		t.Errorf("expected name %q, got %q", "my task", s.TaskName())
	}
	if s.TaskStatus() != TaskStatusIdle {
		t.Errorf("expected status %q, got %q", TaskStatusIdle, s.TaskStatus())
	}
}

func TestTaskSetLastResult(t *testing.T) {
	s := NewTask(TaskConfig{ID: "test-789", Name: "test"})
	if got := s.LastResult(); got != "" {
		t.Errorf("initial LastResult = %q, want empty", got)
	}
	s.SetLastResult("done!")
	if got := s.LastResult(); got != "done!" {
		t.Errorf("LastResult = %q, want done!", got)
	}
}

func TestTaskClaudeID(t *testing.T) {
	s := NewTask(TaskConfig{ID: "test-abc", Name: "test", ClaudeID: "claude-xyz"})
	if got := s.ClaudeID(); got != "claude-xyz" {
		t.Errorf("ClaudeID = %q, want claude-xyz", got)
	}
}

func TestTaskCancelNoProcess(t *testing.T) {
	s := NewTask(TaskConfig{ID: "test-cancel", Name: "test"})
	if err := s.CancelTask(); err != nil {
		t.Errorf("Cancel on idle task: %v", err)
	}
}

func TestTaskFilterEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=1",
		"HOME=/home/test",
	}
	got := filterEnv(env, "CLAUDECODE")
	if len(got) != 2 {
		t.Fatalf("filterEnv: got %d entries, want 2", len(got))
	}
	for _, e := range got {
		if e == "CLAUDECODE=1" {
			t.Error("filterEnv did not remove CLAUDECODE")
		}
	}
}

func TestTaskFilterEnvPartialMatch(t *testing.T) {
	env := []string{"CLAUDECODE=1", "CLAUDECODEOTHER=2"}
	got := filterEnv(env, "CLAUDECODE")
	if len(got) != 1 || got[0] != "CLAUDECODEOTHER=2" {
		t.Errorf("filterEnv partial match: got %v", got)
	}
}

// TestTaskRunSmoke spawns a real claude -p process and runs a trivial
// prompt through Task mode, collecting events from the channel until
// the result arrives. Covers the Task codepath end-to-end: process
// spawn, stdout pipe wiring, NDJSON parsing, event dispatch, and the
// final TaskEventResult.
//
// Gated on CLAUDIA_LIVE=1 (spends API credit) and on the claude
// binary being available.
func TestTaskRunSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	task := NewTask(TaskConfig{
		ID:      "smoke-test",
		Name:    "smoke",
		WorkDir: t.TempDir(),
		Model:   "haiku",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	events, err := task.RunTask(ctx, "respond with: ok")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	var (
		sawInit   bool
		sawResult bool
		resultTxt string
	)
	for ev := range events {
		switch ev.Type {
		case TaskEventInit:
			sawInit = true
			if ev.SessionID == "" {
				t.Error("TaskEventInit missing SessionID")
			}
		case TaskEventResult:
			sawResult = true
			resultTxt = ev.Content
			if ev.DurationMs <= 0 {
				t.Errorf("TaskEventResult DurationMs = %v, want > 0", ev.DurationMs)
			}
		case TaskEventError:
			t.Fatalf("TaskEventError: %s", ev.ErrorMsg)
		}
	}

	if !sawInit {
		t.Error("never saw TaskEventInit")
	}
	if !sawResult {
		t.Error("never saw TaskEventResult")
	}
	if resultTxt == "" {
		t.Error("TaskEventResult content empty")
	}
	t.Logf("result: %q", resultTxt)

	if got := task.TaskStatus(); got != TaskStatusIdle {
		t.Errorf("post-run status = %q, want %q", got, TaskStatusIdle)
	}
}

func TestTaskStop(t *testing.T) {
	s := NewTask(TaskConfig{ID: "test-456", Name: "test"})
	s.StopTask()
	if s.TaskStatus() != TaskStatusStopped {
		t.Errorf("expected status %q, got %q", TaskStatusStopped, s.TaskStatus())
	}
	_, err := s.RunTask(context.Background(), "hello")
	if err == nil {
		t.Error("expected error running stopped task")
	}
}

func TestResolveClaudeBin(t *testing.T) {
	// Build a sandbox directory containing a fake "claude" executable
	// so we can drive the resolver without depending on the host's
	// real claude install.
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	// Snapshot and isolate env: clear CLAUDE_BIN, replace PATH with a
	// directory that doesn't contain claude. Restore on test exit.
	t.Setenv("CLAUDE_BIN", "")
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	t.Run("CLAUDE_BIN absolute path that exists is honoured", func(t *testing.T) {
		t.Setenv("CLAUDE_BIN", fake)
		got, err := resolveClaudeBin()
		if err != nil {
			t.Fatalf("resolveClaudeBin: %v", err)
		}
		if got != fake {
			t.Errorf("got %q, want %q", got, fake)
		}
	})

	t.Run("CLAUDE_BIN relative name is resolved via PATH", func(t *testing.T) {
		// Add the fake's directory to PATH so LookPath finds it.
		t.Setenv("CLAUDE_BIN", "claude")
		t.Setenv("PATH", dir)
		got, err := resolveClaudeBin()
		if err != nil {
			t.Fatalf("resolveClaudeBin: %v", err)
		}
		if got != fake {
			t.Errorf("got %q, want %q", got, fake)
		}
	})

	t.Run("CLAUDE_BIN absolute path that doesn't exist falls through", func(t *testing.T) {
		t.Setenv("CLAUDE_BIN", filepath.Join(dir, "missing-claude"))
		// Put the fake on PATH so LookPath catches it as the fallback.
		t.Setenv("PATH", dir)
		got, err := resolveClaudeBin()
		if err != nil {
			t.Fatalf("resolveClaudeBin: %v", err)
		}
		if got != fake {
			t.Errorf("got %q, want %q (expected PATH fallback)", got, fake)
		}
	})

	t.Run("PATH lookup wins when CLAUDE_BIN unset", func(t *testing.T) {
		t.Setenv("CLAUDE_BIN", "")
		t.Setenv("PATH", dir)
		got, err := resolveClaudeBin()
		if err != nil {
			t.Fatalf("resolveClaudeBin: %v", err)
		}
		if got != fake {
			t.Errorf("got %q, want %q", got, fake)
		}
	})

	t.Run("missing everywhere returns error mentioning CLAUDE_BIN", func(t *testing.T) {
		t.Setenv("CLAUDE_BIN", "")
		t.Setenv("PATH", emptyDir)
		// Steer HOME at a directory with no candidate paths so
		// the known-install-dirs fallback also misses. The
		// hardcoded /opt/homebrew and /usr/local candidates are
		// out of our control, so skip if either exists with a
		// claude binary on this host.
		t.Setenv("HOME", emptyDir)
		for _, sys := range []string{"/opt/homebrew/bin/claude", "/usr/local/bin/claude"} {
			if _, err := os.Stat(sys); err == nil {
				t.Skipf("system has %s; cannot test miss path", sys)
			}
		}
		_, err := resolveClaudeBin()
		if err == nil {
			t.Fatal("expected error when claude is absent from PATH and known dirs")
		}
		if !strings.Contains(err.Error(), "CLAUDE_BIN") {
			t.Errorf("error %q does not mention CLAUDE_BIN", err.Error())
		}
	})
}

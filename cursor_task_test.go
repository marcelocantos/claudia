// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFakeCursorPrint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Cursor print uses a POSIX shell wrapper")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake Cursor print")
	}
	py, err := filepath.Abs("testdata/cursor/print/fake_print.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	script := "#!/bin/sh\nexec python3 \"" + py + "\" \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestParseCursorTaskLineResultUsageCamelCase(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":50,"result":"PING","usage":{"inputTokens":11,"outputTokens":3,"cacheReadTokens":2,"cacheWriteTokens":1}}`)
	evs := ParseCursorTaskLine(line)
	if len(evs) != 1 || evs[0].Type != TaskEventResult {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Content != "PING" {
		t.Fatalf("content = %q", evs[0].Content)
	}
	if evs[0].Usage.InputTokens != 11 || evs[0].Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", evs[0].Usage)
	}
	if evs[0].Usage.CacheReadInputTokens != 2 || evs[0].Usage.CacheCreationInputTokens != 1 {
		t.Fatalf("cache usage = %+v", evs[0].Usage)
	}
}

func TestParseCursorTaskLineError(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":true,"result":"bad model"}`)
	evs := ParseCursorTaskLine(line)
	if len(evs) != 1 || evs[0].Type != TaskEventError || evs[0].ErrorMsg != "bad model" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestCursorTaskArgsResumeAndModel(t *testing.T) {
	args := cursorTaskArgs(taskRunRequest{
		Prompt:    "hi",
		Model:     "auto",
		SessionID: "chat-1",
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--print") || !strings.Contains(joined, "stream-json") {
		t.Fatalf("args missing print/stream-json: %v", args)
	}
	if !argvHolds(args, "auto") || !argvHolds(args, "chat-1") || !argvHolds(args, "--resume") {
		t.Fatalf("args = %v", args)
	}
}

func TestHermeticCursorTaskRun(t *testing.T) {
	bin := writeFakeCursorPrint(t)
	t.Setenv("CURSOR_BIN", bin)
	task := NewTask(TaskConfig{
		Provider: ProviderCursor,
		ID:       "cursor-hermetic",
		WorkDir:  t.TempDir(),
	})
	ch, err := task.Run(t.Context(), "Reply with exactly: pong")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawInit, sawText, sawResult bool
	for ev := range ch {
		switch ev.Type {
		case TaskEventInit:
			sawInit = true
		case TaskEventText:
			if strings.Contains(ev.Content, "pong") {
				sawText = true
			}
		case TaskEventResult:
			sawResult = true
			if ev.Usage.InputTokens == 0 {
				t.Fatalf("result usage empty: %+v", ev.Usage)
			}
		case TaskEventError:
			t.Fatalf("unexpected error event: %+v", ev)
		}
	}
	if !sawInit || !sawText || !sawResult {
		t.Fatalf("saw init=%v text=%v result=%v", sawInit, sawText, sawResult)
	}
}

func TestCursorTaskLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
		t.Skip("CLAUDIA_CURSOR_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCursorBin(); err != nil {
		t.Skipf("cursor agent binary not found: %v", err)
	}
	task := NewTask(TaskConfig{
		Provider: ProviderCursor,
		ID:       "cursor-live-task",
		WorkDir:  t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ch, err := task.Run(ctx, "Reply with exactly: pong")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var text strings.Builder
	var sawResult bool
	for ev := range ch {
		switch ev.Type {
		case TaskEventText:
			text.WriteString(ev.Content)
		case TaskEventResult:
			sawResult = true
			text.WriteString(ev.Content)
		case TaskEventError:
			t.Fatalf("error event: %+v", ev)
		}
	}
	if !sawResult {
		t.Fatal("no TaskEventResult")
	}
	if !strings.Contains(strings.ToLower(text.String()), "pong") {
		t.Fatalf("text %q, want pong", text.String())
	}
}

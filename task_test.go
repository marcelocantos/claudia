// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func TestCodexTaskArgs(t *testing.T) {
	req := taskRunRequest{
		WorkDir:        "/repo",
		Model:          "gpt-5.4",
		SandboxMode:    "read-only",
		ApprovalPolicy: "never",
		Prompt:         "summarize",
	}
	got := codexTaskArgs(req)
	want := []string{
		"--ask-for-approval", "never",
		"--cd", "/repo",
		"--sandbox", "read-only",
		"--model", "gpt-5.4",
		"exec",
		"--json",
		"summarize",
	}
	if !slicesEqual(got, want) {
		t.Errorf("codexTaskArgs = %v, want %v", got, want)
	}
}

func TestCodexTaskArgsResume(t *testing.T) {
	got := codexTaskArgs(taskRunRequest{SessionID: "thread-123", Prompt: "continue"})
	want := []string{"exec", "resume", "--json", "thread-123", "continue"}
	if !slicesEqual(got, want) {
		t.Errorf("codexTaskArgs resume = %v, want %v", got, want)
	}
}

func TestCodexTaskParser(t *testing.T) {
	parser := codexTaskParser{}
	lines := readFixtureLines(t, "testdata/codex/exec/success.jsonl")

	var events []TaskEvent
	for _, line := range lines {
		events = append(events, parser.Parse([]byte(line))...)
	}
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5: %#v", len(events), events)
	}
	if events[0].Type != TaskEventInit || events[0].SessionID == "" {
		t.Errorf("init event = %#v", events[0])
	}
	if events[1].Type != TaskEventToolUse || events[1].ToolName != "command_execution" {
		t.Errorf("command event = %#v", events[1])
	}
	if events[2].Type != TaskEventText || events[2].Content == "" {
		t.Errorf("text event = %#v", events[2])
	}
	if events[3].Type != TaskEventText || events[3].Content != "Final answer." {
		t.Errorf("final text event = %#v", events[3])
	}
	result := events[4]
	if result.Type != TaskEventResult {
		t.Fatalf("result event = %#v", result)
	}
	if result.Content != "Final answer." {
		t.Errorf("result content = %q, want final agent message", result.Content)
	}
	if result.Usage.InputTokens != 24763 {
		t.Errorf("input tokens = %d, want 24763", result.Usage.InputTokens)
	}
	if result.Usage.CacheReadInputTokens != 24448 {
		t.Errorf("cached input tokens = %d, want 24448", result.Usage.CacheReadInputTokens)
	}
	if result.Usage.OutputTokens != 122 {
		t.Errorf("output tokens = %d, want 122", result.Usage.OutputTokens)
	}
}

func TestCodexTaskSuccessOracleRejectsFaults(t *testing.T) {
	lines := readFixtureLines(t, "testdata/codex/exec/success.jsonl")
	if err := codexTaskSuccessOracle(lines); err != nil {
		t.Fatalf("codexTaskSuccessOracle(success): %v", err)
	}

	droppedSessionID := append([]string(nil), lines[1:]...)
	if err := codexTaskSuccessOracle(droppedSessionID); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("dropped session id error = %v, want session failure", err)
	}

	wrongFinalMessage := append([]string(nil), lines...)
	wrongFinalMessage[4] = strings.Replace(wrongFinalMessage[4], "Final answer.", "Stale answer.", 1)
	if err := codexTaskSuccessOracle(wrongFinalMessage); err == nil || !strings.Contains(err.Error(), "final") {
		t.Fatalf("wrong final-message error = %v, want final failure", err)
	}

	malformedUsage := append([]string(nil), lines...)
	malformedUsage[5] = strings.Replace(malformedUsage[5], `"cached_input_tokens":24448`, `"cached_input_tokens":122`, 1)
	if err := codexTaskSuccessOracle(malformedUsage); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("malformed usage error = %v, want usage failure", err)
	}
}

func codexTaskSuccessOracle(lines []string) error {
	parser := codexTaskParser{}
	var (
		sawSessionID bool
		result       TaskEvent
	)
	for _, line := range lines {
		for _, ev := range parser.Parse([]byte(line)) {
			if ev.Type == TaskEventInit && ev.SessionID != "" {
				sawSessionID = true
			}
			if ev.Type == TaskEventResult {
				result = ev
			}
		}
	}
	if !sawSessionID {
		return errors.New("missing session id")
	}
	if result.Type != TaskEventResult {
		return errors.New("missing final result")
	}
	if result.Content != "Final answer." {
		return errors.New("wrong final message")
	}
	if result.Usage.InputTokens != 24763 ||
		result.Usage.CacheReadInputTokens != 24448 ||
		result.Usage.OutputTokens != 122 {
		return errors.New("malformed usage accounting")
	}
	return nil
}

func TestCodexTaskParserErrors(t *testing.T) {
	parser := codexTaskParser{}
	for _, fixture := range []string{
		"testdata/codex/exec/failure.jsonl",
		"testdata/codex/exec/error.jsonl",
	} {
		var events []TaskEvent
		for _, line := range readFixtureLines(t, fixture) {
			events = append(events, parser.Parse([]byte(line))...)
		}
		if len(events) == 0 {
			t.Fatalf("no events for %s", fixture)
		}
		ev := events[len(events)-1]
		if ev.Type != TaskEventError || !ev.IsError || ev.ErrorMsg == "" {
			t.Errorf("error event = %#v", ev)
		}
	}
}

func TestCodexTaskParserMalformedFixture(t *testing.T) {
	parser := codexTaskParser{}
	for _, line := range readFixtureLines(t, "testdata/codex/exec/malformed.jsonl") {
		events := parser.Parse([]byte(line))
		if len(events) != 0 {
			t.Errorf("events for malformed/unknown line %q = %#v, want none", line, events)
		}
	}
}

func TestGrokTaskArgs(t *testing.T) {
	req := taskRunRequest{
		WorkDir: "/repo",
		Model:   "grok-4",
		Prompt:  "summarize",
	}
	got := grokTaskArgs(req)
	want := []string{
		"-p", "summarize",
		"--output-format", "streaming-json",
		"--permission-mode", "bypassPermissions",
		"--cwd", "/repo",
		"--model", "grok-4",
	}
	if !slicesEqual(got, want) {
		t.Errorf("grokTaskArgs = %v, want %v", got, want)
	}
}

func TestGrokTaskArgsResume(t *testing.T) {
	got := grokTaskArgs(taskRunRequest{SessionID: "sess-123", Prompt: "continue"})
	want := []string{
		"-p", "continue",
		"--output-format", "streaming-json",
		"--permission-mode", "bypassPermissions",
		"--resume", "sess-123",
	}
	if !slicesEqual(got, want) {
		t.Errorf("grokTaskArgs resume = %v, want %v", got, want)
	}
}

func TestGrokTaskParser(t *testing.T) {
	parser := grokTaskParser{}
	var events []TaskEvent
	for _, line := range readFixtureLines(t, "testdata/grok/exec/success.jsonl") {
		events = append(events, parser.Parse([]byte(line))...)
	}
	// text, text, init, result — thought ignored
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4: %#v", len(events), events)
	}
	if events[0].Type != TaskEventText || events[0].Content != "Final " {
		t.Errorf("text[0] = %#v", events[0])
	}
	if events[1].Type != TaskEventText || events[1].Content != "answer." {
		t.Errorf("text[1] = %#v", events[1])
	}
	if events[2].Type != TaskEventInit || events[2].SessionID != "019f4f39-0ad7-74c1-942b-32557bd3c302" {
		t.Errorf("init = %#v", events[2])
	}
	if events[3].Type != TaskEventResult || events[3].Content != "Final answer." {
		t.Errorf("result = %#v", events[3])
	}
}

func TestGrokTaskSuccessOracleRejectsFaults(t *testing.T) {
	lines := readFixtureLines(t, "testdata/grok/exec/success.jsonl")
	if err := grokTaskSuccessOracle(lines); err != nil {
		t.Fatalf("grokTaskSuccessOracle(success): %v", err)
	}

	droppedSessionID := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, `"type":"end"`) {
			line = `{"type":"end","stopReason":"EndTurn","requestId":"x"}`
		}
		droppedSessionID = append(droppedSessionID, line)
	}
	if err := grokTaskSuccessOracle(droppedSessionID); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("dropped session id error = %v, want session failure", err)
	}

	wrongFinal := append([]string(nil), lines...)
	wrongFinal[2] = `{"type":"text","data":"Stale."}`
	if err := grokTaskSuccessOracle(wrongFinal); err == nil || !strings.Contains(err.Error(), "final") {
		t.Fatalf("wrong final-message error = %v, want final failure", err)
	}
}

func grokTaskSuccessOracle(lines []string) error {
	parser := grokTaskParser{}
	var (
		sawSessionID bool
		result       TaskEvent
	)
	for _, line := range lines {
		for _, ev := range parser.Parse([]byte(line)) {
			if ev.Type == TaskEventInit && ev.SessionID != "" {
				sawSessionID = true
			}
			if ev.Type == TaskEventResult {
				result = ev
			}
		}
	}
	if !sawSessionID {
		return errors.New("missing session id")
	}
	if result.Type != TaskEventResult {
		return errors.New("missing final result")
	}
	if result.Content != "Final answer." {
		return errors.New("wrong final message")
	}
	return nil
}

func TestGrokTaskParserErrors(t *testing.T) {
	parser := grokTaskParser{}
	var events []TaskEvent
	for _, line := range readFixtureLines(t, "testdata/grok/exec/error.jsonl") {
		events = append(events, parser.Parse([]byte(line))...)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Type != TaskEventError || !events[0].IsError || events[0].ErrorMsg == "" {
		t.Errorf("error event = %#v", events[0])
	}
}

func TestGrokTaskParserMalformedFixture(t *testing.T) {
	parser := grokTaskParser{}
	var events []TaskEvent
	for _, line := range readFixtureLines(t, "testdata/grok/exec/malformed.jsonl") {
		events = append(events, parser.Parse([]byte(line))...)
	}
	// malformed + unknown ignored; text + init + result remain
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3: %#v", len(events), events)
	}
	if events[0].Type != TaskEventText || events[0].Content != "ok" {
		t.Errorf("text = %#v", events[0])
	}
	if events[1].Type != TaskEventInit || events[1].SessionID != "sess-malformed" {
		t.Errorf("init = %#v", events[1])
	}
	if events[2].Type != TaskEventResult || events[2].Content != "ok" {
		t.Errorf("result = %#v", events[2])
	}
}

func TestTaskBackendForProvider(t *testing.T) {
	if _, ok := taskBackendForProvider("").(claudeTaskBackend); !ok {
		t.Error("empty provider did not select claudeTaskBackend")
	}
	if _, ok := taskBackendForProvider(ProviderCodex).(codexTaskBackend); !ok {
		t.Error("ProviderCodex did not select codexTaskBackend")
	}
	if _, ok := taskBackendForProvider(ProviderGrok).(grokTaskBackend); !ok {
		t.Error("ProviderGrok did not select grokTaskBackend")
	}
	caps := taskBackendForProvider(ProviderGrok).Capabilities()
	if !caps.Task || !caps.Resume || caps.Cost || caps.Session {
		t.Errorf("ProviderGrok capabilities = %+v, want Task+Resume only", caps)
	}
	backend := taskBackendForProvider(Provider("bogus"))
	if _, ok := backend.(errorTaskBackend); !ok {
		t.Fatalf("unknown provider backend = %T, want errorTaskBackend", backend)
	}
	if _, err := backend.RunTask(context.Background(), taskRunRequest{}); err == nil {
		t.Error("unknown provider RunTask returned nil error")
	}
}

// TestGrokPublicTaskEntryPath drives NewTask(ProviderGrok) and a fixture-
// driven parser oracle on the real mapping functions used by the shipped
// backend (not a reimplemented parser).
func TestGrokPublicTaskEntryPath(t *testing.T) {
	task := NewTask(TaskConfig{
		Provider: ProviderGrok,
		ID:       "public-entry",
		WorkDir:  t.TempDir(),
	})
	if _, ok := task.backend.(grokTaskBackend); !ok {
		t.Fatalf("NewTask(ProviderGrok).backend = %T, want grokTaskBackend", task.backend)
	}
	lines := readFixtureLines(t, "testdata/grok/exec/success.jsonl")
	if err := grokTaskSuccessOracle(lines); err != nil {
		t.Fatalf("success oracle: %v", err)
	}
	// Missing end event → no session id
	if err := grokTaskSuccessOracle(lines[:len(lines)-1]); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("truncated stream error = %v, want session failure", err)
	}
	// Explicit error event path
	errLines := readFixtureLines(t, "testdata/grok/exec/error.jsonl")
	parser := grokTaskParser{}
	var sawErr bool
	for _, line := range errLines {
		for _, ev := range parser.Parse([]byte(line)) {
			if ev.Type == TaskEventError && ev.IsError && ev.ErrorMsg != "" {
				sawErr = true
			}
		}
	}
	if !sawErr {
		t.Fatal("error fixture did not produce TaskEventError via shipped parser")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readFixtureLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestNewTask(t *testing.T) {
	s := NewTask(TaskConfig{
		ID:      "test-123",
		Name:    "my task",
		WorkDir: "/tmp",
		Model:   "sonnet",
	})
	if s.ID() != "test-123" {
		t.Errorf("expected ID %q, got %q", "test-123", s.ID())
	}
	if s.Name() != "my task" {
		t.Errorf("expected name %q, got %q", "my task", s.Name())
	}
	if s.Status() != TaskStatusIdle {
		t.Errorf("expected status %q, got %q", TaskStatusIdle, s.Status())
	}
}

func TestTaskRestoreLastResult(t *testing.T) {
	// LastResult passed via TaskConfig is preserved (used to restore
	// the most recent result text when re-hydrating a Task from
	// persisted state).
	s := NewTask(TaskConfig{ID: "test-789", Name: "test"})
	if got := s.LastResult(); got != "" {
		t.Errorf("initial LastResult = %q, want empty", got)
	}
	s2 := NewTask(TaskConfig{ID: "test-790", Name: "test", LastResult: "done!"})
	if got := s2.LastResult(); got != "done!" {
		t.Errorf("LastResult = %q, want done!", got)
	}
}

func TestTaskClaudeID(t *testing.T) {
	s := NewTask(TaskConfig{ID: "test-abc", Name: "test", ClaudeID: "claude-xyz"})
	if got := s.ClaudeID(); got != "claude-xyz" {
		t.Errorf("ClaudeID = %q, want claude-xyz", got)
	}
}

type fakeTaskBackend struct {
	name         string
	events       []TaskEvent
	ready        chan struct{}
	release      chan struct{}
	interruptErr error

	mu              sync.Mutex
	requests        []taskRunRequest
	interruptCalled int
}

func (b *fakeTaskBackend) RunTask(ctx context.Context, req taskRunRequest) (*taskRun, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()

	ch := make(chan TaskEvent, 16)
	go func() {
		defer close(ch)
		if b.ready != nil {
			close(b.ready)
		}
		if b.release != nil {
			select {
			case <-b.release:
			case <-ctx.Done():
				return
			}
		}
		for _, ev := range b.events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return &taskRun{
		events: ch,
		interrupt: func() error {
			b.mu.Lock()
			b.interruptCalled++
			b.mu.Unlock()
			return b.interruptErr
		},
	}, nil
}

func (b *fakeTaskBackend) Capabilities() providerCapabilities {
	return providerCapabilities{
		Task:   true,
		Resume: true,
		Cost:   b.name == "fake-claude",
	}
}

func (b *fakeTaskBackend) request(t *testing.T) taskRunRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) != 1 {
		t.Fatalf("%s requests = %d, want 1", b.name, len(b.requests))
	}
	return b.requests[0]
}

func (b *fakeTaskBackend) interrupts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.interruptCalled
}

func TestTaskRunUsesInjectedBackendLifecycle(t *testing.T) {
	for _, provider := range []string{"fake-claude", "fake-codex", "fake-grok"} {
		t.Run(provider, func(t *testing.T) {
			backend := &fakeTaskBackend{
				name: provider,
				events: []TaskEvent{
					{Type: TaskEventInit, SessionID: provider + "-session"},
					{Type: TaskEventText, Content: "hello"},
					{Type: TaskEventResult, Content: "done"},
				},
			}
			task := newTaskWithBackend(TaskConfig{
				ID:      provider + "-task",
				Name:    provider,
				WorkDir: "/tmp/" + provider,
				Model:   "test-model",
			}, backend)

			events, err := task.Run(context.Background(), "test prompt")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var got []TaskEvent
			for ev := range events {
				got = append(got, ev)
			}

			if len(got) != len(backend.events) {
				t.Fatalf("events = %d, want %d", len(got), len(backend.events))
			}
			if got[1].Content != "hello" {
				t.Errorf("text event content = %q, want hello", got[1].Content)
			}
			if task.ClaudeID() != provider+"-session" {
				t.Errorf("session id = %q, want %q", task.ClaudeID(), provider+"-session")
			}
			if task.LastResult() != "done" {
				t.Errorf("LastResult = %q, want done", task.LastResult())
			}
			if task.Status() != TaskStatusIdle {
				t.Errorf("Status = %q, want %q", task.Status(), TaskStatusIdle)
			}
			caps := backend.Capabilities()
			if !caps.Task || !caps.Resume {
				t.Errorf("capabilities = %+v, want task+resume", caps)
			}
			if (provider == "fake-codex" || provider == "fake-grok") && caps.Cost {
				t.Errorf("%s capabilities = %+v, want cost unsupported", provider, caps)
			}

			req := backend.request(t)
			if req.WorkDir != "/tmp/"+provider {
				t.Errorf("WorkDir = %q, want %q", req.WorkDir, "/tmp/"+provider)
			}
			if req.Model != "test-model" {
				t.Errorf("Model = %q, want test-model", req.Model)
			}
			if req.SessionID != "" {
				t.Errorf("SessionID = %q, want empty", req.SessionID)
			}
			if req.Prompt != "test prompt" {
				t.Errorf("Prompt = %q, want test prompt", req.Prompt)
			}
		})
	}
}

func TestTaskRunPassesExistingSessionToBackend(t *testing.T) {
	backend := &fakeTaskBackend{events: []TaskEvent{{Type: TaskEventResult, Content: "done"}}}
	task := newTaskWithBackend(TaskConfig{
		ID:       "resume-task",
		ClaudeID: "existing-session",
	}, backend)

	events, err := task.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}

	if req := backend.request(t); req.SessionID != "existing-session" {
		t.Errorf("SessionID = %q, want existing-session", req.SessionID)
	}
	if task.ClaudeID() != "existing-session" {
		t.Errorf("ClaudeID = %q, want existing-session", task.ClaudeID())
	}
}

func TestTaskCancelDelegatesToBackendInterrupt(t *testing.T) {
	wantErr := errors.New("interrupt failed")
	backend := &fakeTaskBackend{
		ready:        make(chan struct{}),
		release:      make(chan struct{}),
		interruptErr: wantErr,
	}
	task := newTaskWithBackend(TaskConfig{ID: "cancel-task"}, backend)

	events, err := task.Run(context.Background(), "wait")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-backend.ready

	if err := task.Cancel(); !errors.Is(err, wantErr) {
		t.Fatalf("Cancel error = %v, want %v", err, wantErr)
	}
	if got := backend.interrupts(); got != 1 {
		t.Errorf("interrupt calls = %d, want 1", got)
	}
	close(backend.release)
	for range events {
	}
}

func TestTaskCancelNoProcess(t *testing.T) {
	s := NewTask(TaskConfig{ID: "test-cancel", Name: "test"})
	if err := s.Cancel(); err != nil {
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

	events, err := task.Run(ctx, "respond with: ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
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

	if got := task.Status(); got != TaskStatusIdle {
		t.Errorf("post-run status = %q, want %q", got, TaskStatusIdle)
	}
}

// TestGrokTaskRunSmoke spawns a real grok headless process and runs a
// trivial prompt through Task mode. Gated on CLAUDIA_GROK_LIVE because it
// uses local Grok credentials and may contact xAI.
func TestGrokTaskRunSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveGrokBin(); err != nil {
		t.Skipf("grok binary not found: %v", err)
	}

	workDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	task := NewTask(TaskConfig{
		ID:       "grok-smoke-test",
		Name:     "grok-smoke",
		Provider: ProviderGrok,
		WorkDir:  workDir,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	events, err := task.Run(ctx, "respond with exactly: ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawResult bool
	var sessionID string
	for ev := range events {
		switch ev.Type {
		case TaskEventInit:
			sessionID = ev.SessionID
		case TaskEventResult:
			sawResult = true
			if ev.Content == "" {
				t.Error("TaskEventResult content empty")
			}
		case TaskEventError:
			t.Fatalf("TaskEventError: %s", ev.ErrorMsg)
		}
	}
	if !sawResult {
		t.Error("never saw TaskEventResult")
	}
	if sessionID == "" && task.ClaudeID() == "" {
		t.Error("never captured Grok session id")
	}
}

// TestCodexTaskRunSmoke spawns a real codex exec --json process and runs a
// trivial prompt through Task mode. It is gated separately from
// CLAUDIA_LIVE because it uses Codex credentials and may contact OpenAI.
func TestCodexTaskRunSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCodexBin(); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}

	workDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	task := NewTask(TaskConfig{
		ID:             "codex-smoke-test",
		Name:           "codex-smoke",
		Provider:       ProviderCodex,
		WorkDir:        workDir,
		SandboxMode:    "read-only",
		ApprovalPolicy: "never",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	events, err := task.Run(ctx, "respond with: ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawResult bool
	for ev := range events {
		switch ev.Type {
		case TaskEventResult:
			sawResult = true
			if ev.Content == "" {
				t.Error("TaskEventResult content empty")
			}
		case TaskEventError:
			t.Fatalf("TaskEventError: %s", ev.ErrorMsg)
		}
	}
	if !sawResult {
		t.Error("never saw TaskEventResult")
	}
}

func TestTaskStop(t *testing.T) {
	s := NewTask(TaskConfig{ID: "test-456", Name: "test"})
	s.Stop()
	if s.Status() != TaskStatusStopped {
		t.Errorf("expected status %q, got %q", TaskStatusStopped, s.Status())
	}
	_, err := s.Run(context.Background(), "hello")
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

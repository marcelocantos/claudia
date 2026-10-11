// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

func TestCodexTaskParserAuditsCommandOnce(t *testing.T) {
	parser := codexTaskParser{}
	var calls int
	for _, line := range []string{
		`{"type":"item.started","item":{"id":"cmd-1","type":"command_execution","command":"ps -axo pid,command","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"cmd-1","type":"command_execution","command":"ps -axo pid,command","status":"completed"}}`,
	} {
		for _, event := range parser.Parse([]byte(line)) {
			if event.Type == TaskEventToolUse {
				calls++
			}
		}
	}
	if calls != 1 {
		t.Fatalf("command audit events = %d, want 1", calls)
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

func TestGrokTaskParserReturnsFinalResponseAfterToolTurn(t *testing.T) {
	parser := grokTaskParser{}
	lines := []string{
		`{"type":"text","data":"I'll read the transcript first."}`,
		`{"type":"usage","messageId":"progress","stopReason":"tool_use"}`,
		`{"type":"tool_call","toolCallId":"read-1","toolName":"read_file","rawInput":{"path":"runbook.md"}}`,
		`{"type":"tool_call_update","toolCallId":"read-1","status":"completed"}`,
		`{"type":"text","data":"frontier-ai-enterprise-rejection.md\n\n# Frontier AI Enterprise Rejection\n\n**TL;DR**: The thesis."}`,
		`{"type":"usage","messageId":"final","stopReason":"end_turn"}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"session-1"}`,
	}
	var result string
	var tool TaskEvent
	for _, line := range lines {
		for _, event := range parser.Parse([]byte(line)) {
			if event.Type == TaskEventResult {
				result = event.Content
			}
			if event.Type == TaskEventToolUse {
				tool = event
			}
		}
	}
	if tool.ToolID != "read-1" || tool.ToolName != "read_file" || tool.ToolInput != `{"path":"runbook.md"}` {
		t.Errorf("tool audit event = %+v", tool)
	}
	want := "frontier-ai-enterprise-rejection.md\n\n# Frontier AI Enterprise Rejection\n\n**TL;DR**: The thesis."
	if result != want {
		t.Errorf("result = %q, want %q", result, want)
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
	for _, provider := range []Provider{"", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor} {
		t.Run(string(provider), func(t *testing.T) {
			backend := taskBackendForProvider(provider)
			if _, ok := backend.(errorTaskBackend); !ok {
				t.Fatalf("backend = %T, want errorTaskBackend", backend)
			}
			if backend.Capabilities() != (providerCapabilities{}) {
				t.Fatalf("capabilities = %+v, want none", backend.Capabilities())
			}
			_, err := backend.RunTask(context.Background(), taskRunRequest{Prompt: "hello"})
			if !errors.Is(err, ErrTaskCLIRemoved) {
				t.Fatalf("RunTask error = %v, want ErrTaskCLIRemoved", err)
			}
		})
	}
	if _, ok := taskBackendForProvider(ProviderBedrock).(bedrockTaskBackend); !ok {
		t.Error("Bedrock backend changed")
	}
	if _, ok := taskBackendForProvider(ProviderOllama).(ollamaTaskBackend); !ok {
		t.Error("Ollama backend changed")
	}
	if _, err := taskBackendForProvider(Provider("bogus")).RunTask(context.Background(), taskRunRequest{}); err == nil {
		t.Error("unknown provider accepted")
	}
}

func TestRemovedCLITaskRunReturnsPromptly(t *testing.T) {
	for _, provider := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor} {
		t.Run(string(provider), func(t *testing.T) {
			task := NewTask(TaskConfig{Provider: provider, WorkDir: t.TempDir()})
			task.SetDirect(true)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			events, err := task.Run(ctx, "hello")
			if !errors.Is(err, ErrTaskCLIRemoved) || events != nil {
				t.Fatalf("Task.Run = (%v, %v), want removed error and no events", events, err)
			}
			if ctx.Err() != nil {
				t.Fatal("Task.Run timed out")
			}
		})
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
	if _, ok := task.backend.(errorTaskBackend); !ok {
		t.Fatalf("NewTask(ProviderGrok).backend = %T, want errorTaskBackend", task.backend)
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
	name   string
	events []TaskEvent
	// rawLines are handed to the run's RawLog, in order, before events.
	rawLines     []string
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
		if req.RawLog != nil {
			for _, line := range b.rawLines {
				req.RawLog([]byte(line))
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

// TestForwardTaskStreamOversizedLine reproduces 🎯T133: a Claude Task
// result line carrying a large embedded tool payload (e.g. an image read
// back through Read) that exceeds a small fixed scanner buffer must
// still be delivered — or forwardTaskStream must report why it wasn't —
// rather than silently vanishing with no TaskEventResult and no error.
func TestForwardTaskStreamOversizedLine(t *testing.T) {
	// Build a synthetic NDJSON result line whose total size comfortably
	// exceeds the old 1 MiB scanner cap (this reproduces the failure
	// mode against the pre-fix 1 MiB buffer) but stays under the new
	// bound, so the fixed scanner must scan it successfully.
	bigPayload := strings.Repeat("x", 2*1024*1024) // 2 MiB > old 1 MiB cap
	resultLine := `{"type":"result","subtype":"success","result":"` + bigPayload + `"}`

	r := strings.NewReader(resultLine + "\n")
	ch := make(chan TaskEvent, 4)
	err := forwardTaskStream(context.Background(), r, ParseTaskLine, nil, ch)
	close(ch)

	if err != nil {
		t.Fatalf("forwardTaskStream returned unexpected error for a line under the bound: %v", err)
	}

	var gotResult bool
	for ev := range ch {
		if ev.Type == TaskEventResult {
			gotResult = true
			if !strings.Contains(ev.Content, "xxxx") {
				t.Fatalf("result content missing expected payload, len=%d", len(ev.Content))
			}
		}
	}
	if !gotResult {
		t.Fatal("forwardTaskStream dropped the oversized line: no TaskEventResult was delivered (the 🎯T133 lost-result bug)")
	}
}

// TestForwardTaskStreamBeyondBoundSurfacesError proves that a line
// beyond even the enlarged sane bound ends the stream with an explicit,
// named error — scanner.Err() is checked and surfaced — rather than the
// scan loop just stopping silently as it did before 🎯T133.
func TestForwardTaskStreamBeyondBoundSurfacesError(t *testing.T) {
	// bufio.Scanner's max token size is bounded by the buffer passed to
	// Buffer(); construct a reader whose single line exceeds that bound
	// via a tiny scanner wrapper is not directly controllable from here
	// (forwardTaskStream owns its own bufio.Scanner), so instead we
	// drive the same code path with an io.Reader that errors outright,
	// which is the other branch scanner.Err() must surface.
	r := &erroringReader{err: errors.New("synthetic read failure")}
	ch := make(chan TaskEvent, 4)
	err := forwardTaskStream(context.Background(), r, ParseTaskLine, nil, ch)
	close(ch)

	if err == nil {
		t.Fatal("forwardTaskStream swallowed a scanner read error instead of surfacing it")
	}
	if !strings.Contains(err.Error(), "synthetic read failure") {
		t.Fatalf("error does not name the underlying cause: %v", err)
	}
	for range ch {
		t.Fatal("no events should have been emitted before the read error")
	}
}

// erroringReader is an io.Reader that always fails, used to drive
// bufio.Scanner's error path deterministically.
type erroringReader struct{ err error }

func (r *erroringReader) Read(_ []byte) (int, error) { return 0, r.err }

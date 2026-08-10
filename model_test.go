// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// These oracles pin the "no silent model failure" contract (🎯T16): the
// resolved model is observable per backend, and an unusable model is loud.

func TestParseEventCapturesResolvedModel(t *testing.T) {
	line := `{"type":"assistant","message":{"model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}]}}`
	ev := parseEvent(line)
	if ev.Model != "claude-opus-5" {
		t.Fatalf("Event.Model = %q, want claude-opus-5", ev.Model)
	}
	if ev.IsError {
		t.Fatal("normal assistant event must not set IsError")
	}
	if m := parseEvent(`{"type":"system","subtype":"turn_duration"}`).Model; m != "" {
		t.Fatalf("non-assistant Event.Model = %q, want empty", m)
	}
}

func TestParseEventModelNotFoundIsError(t *testing.T) {
	line := `{"type":"assistant","message":{"model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"There's an issue with the selected model (definitely-not-a-real-model-xyzzy)."}]},"error":"model_not_found","is_api_error_message":true}`
	ev := parseEvent(line)
	if ev.Model != "<synthetic>" {
		t.Fatalf("Event.Model = %q, want <synthetic>", ev.Model)
	}
	if !ev.IsError {
		t.Fatal("model_not_found assistant must set IsError")
	}
	if !strings.Contains(ev.Text, "definitely-not-a-real-model-xyzzy") {
		t.Fatalf("error text must name the model, got %q", ev.Text)
	}
}

func TestParseTaskSystemCapturesResolvedModel(t *testing.T) {
	evs := ParseTaskLine([]byte(`{"type":"system","subtype":"init","session_id":"abc","model":"claude-fable-5"}`))
	if len(evs) != 1 || evs[0].Type != TaskEventInit {
		t.Fatalf("want one init event, got %+v", evs)
	}
	if evs[0].Model != "claude-fable-5" {
		t.Fatalf("TaskEvent.Model = %q, want claude-fable-5", evs[0].Model)
	}
}

func TestParseTaskResultErrorIsLoud(t *testing.T) {
	evs := ParseTaskLine([]byte(`{"type":"result","subtype":"error","errors":[{"message":"model not found: bogus"}]}`))
	if len(evs) != 1 || evs[0].Type != TaskEventError || !evs[0].IsError {
		t.Fatalf("want a loud TaskEventError, got %+v", evs)
	}
	if evs[0].ErrorMsg == "" {
		t.Fatal("error must name the failure, got empty ErrorMsg")
	}
}

func TestParseTaskResultIsErrorTrueDespiteSuccessSubtype(t *testing.T) {
	// Live shape from Claude Code 2.1.x: subtype "success" + is_error true.
	line := `{"type":"result","subtype":"success","is_error":true,"result":"There's an issue with the selected model (definitely-not-a-real-model-xyzzy)."}`
	evs := ParseTaskLine([]byte(line))
	if len(evs) != 1 || evs[0].Type != TaskEventError || !evs[0].IsError {
		t.Fatalf("want TaskEventError for is_error+success subtype, got %+v", evs)
	}
	if !strings.Contains(evs[0].ErrorMsg, "definitely-not-a-real-model-xyzzy") {
		t.Fatalf("ErrorMsg must name the model, got %q", evs[0].ErrorMsg)
	}
}

func TestParseTaskModelNotFoundFixture(t *testing.T) {
	var initModel string
	var sawError bool
	var errMsg string
	for _, line := range readFixtureLines(t, "testdata/claude/exec/model_not_found.jsonl") {
		for _, ev := range ParseTaskLine([]byte(line)) {
			switch ev.Type {
			case TaskEventInit:
				initModel = ev.Model
			case TaskEventError:
				sawError = true
				errMsg = ev.ErrorMsg
			case TaskEventResult:
				t.Fatalf("must not treat model_not_found result as TaskEventResult: %+v", ev)
			}
		}
	}
	if initModel != "definitely-not-a-real-model-xyzzy" {
		t.Fatalf("init model = %q, want the requested unusable id", initModel)
	}
	if !sawError {
		t.Fatal("fixture must produce TaskEventError")
	}
	if !strings.Contains(errMsg, "definitely-not-a-real-model-xyzzy") {
		t.Fatalf("ErrorMsg must name the model, got %q", errMsg)
	}
}

func TestWaitForResponseModelNotFoundIsLoud(t *testing.T) {
	a := &Agent{eventSubs: make(map[int64]EventFunc)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		text, err := a.WaitForResponse(ctx)
		ch <- result{text, err}
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		a.mu.Lock()
		n := len(a.eventSubs)
		a.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WaitForResponse handler never installed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	line := `{"type":"assistant","message":{"model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"There's an issue with the selected model (definitely-not-a-real-model-xyzzy)."}]},"error":"model_not_found","is_api_error_message":true}`
	a.publishEvent(parseEvent(line))

	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatalf("WaitForResponse must return error, got text=%q", r.text)
		}
		if !strings.Contains(r.err.Error(), "definitely-not-a-real-model-xyzzy") {
			t.Fatalf("error must name the model, got %v", r.err)
		}
		if a.Model() != "<synthetic>" {
			t.Fatalf("Agent.Model() = %q, want <synthetic>", a.Model())
		}
	case <-ctx.Done():
		t.Fatal("WaitForResponse hung on model_not_found — must fail loud, not timeout")
	}
}

func TestCodexAppServerThreadStartReportsModel(t *testing.T) {
	var model string
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/thread-start.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !ok {
			continue
		}
		if ev.Method == "thread/start" && ev.Model != "" {
			model = ev.Model
		}
		if agentEv, ok := ev.agentEvent(); ok && agentEv.Model != "" {
			if agentEv.Model != "gpt-5.5" {
				t.Fatalf("agentEvent.Model = %q, want gpt-5.5", agentEv.Model)
			}
		}
	}
	if model != "gpt-5.5" {
		t.Fatalf("thread/start model = %q, want gpt-5.5", model)
	}
}

func TestCodexAppServerFailureIsErrorEvent(t *testing.T) {
	var saw bool
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/failure.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil || !ok {
			continue
		}
		agentEv, ok := ev.agentEvent()
		if !ok {
			continue
		}
		if agentEv.IsError {
			saw = true
			if agentEv.Text != "model failed" {
				t.Fatalf("error text = %q, want model failed", agentEv.Text)
			}
		}
	}
	if !saw {
		t.Fatal("failure fixture must produce Event.IsError for WaitForResponse")
	}
}

func TestHermeticBedrockTaskReportsResolvedModel(t *testing.T) {
	fake := &fakeBedrockStreamer{
		events: []TaskEvent{
			{Type: TaskEventText, Content: "hi"},
			{Type: TaskEventResult, Content: "hi"},
		},
	}
	t.Setenv(bedrockRegionEnv, "us-east-1")
	task := newTaskWithBackend(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-model-obs",
		Model:    "anthropic.claude-test-id",
	}, bedrockTaskBackend{streamer: fake})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := task.Run(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	var sawInit bool
	for ev := range ch {
		if ev.Type == TaskEventInit {
			sawInit = true
			if ev.Model != "anthropic.claude-test-id" {
				t.Fatalf("init Model = %q", ev.Model)
			}
		}
	}
	if !sawInit {
		t.Fatal("bedrock must emit TaskEventInit with resolved model")
	}
	if got := task.Model(); got != "anthropic.claude-test-id" {
		t.Fatalf("Task.Model() = %q, want anthropic.claude-test-id", got)
	}
}

// TestModelObservableLive: alias "fable" must resolve to the full id via
// Task.Model() — catches silent fallback. Gated on CLAUDIA_LIVE.
func TestModelObservableLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveClaudeBin(); err != nil {
		t.Skip("claude binary not found")
	}
	task := NewTask(TaskConfig{ID: "model-obs", WorkDir: t.TempDir(), Model: "fable"})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ch, err := task.Run(ctx, "reply with only: ok")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for range ch {
	}
	if got := task.Model(); got != "claude-fable-5" {
		t.Fatalf("Task.Model() = %q, want claude-fable-5 — alias 'fable' must resolve, not silently fall back", got)
	}
}

// TestModelNotFoundLiveFailLoud: unusable model must surface as TaskEventError
// naming the model. Gated on CLAUDIA_LIVE.
func TestModelNotFoundLiveFailLoud(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set")
	}
	if _, err := resolveClaudeBin(); err != nil {
		t.Skip("claude binary not found")
	}
	const bad = "definitely-not-a-real-model-xyzzy"
	task := NewTask(TaskConfig{ID: "model-nf", WorkDir: t.TempDir(), Model: bad})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ch, err := task.Run(ctx, "say hi")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var sawErr bool
	var errMsg string
	for ev := range ch {
		if ev.Type == TaskEventError {
			sawErr = true
			errMsg = ev.ErrorMsg
		}
		if ev.Type == TaskEventResult {
			t.Fatalf("unusable model must not emit TaskEventResult: %+v", ev)
		}
	}
	if !sawErr {
		t.Fatal("expected TaskEventError for unusable model")
	}
	if !strings.Contains(errMsg, bad) {
		t.Fatalf("ErrorMsg must name %q, got %q", bad, errMsg)
	}
}

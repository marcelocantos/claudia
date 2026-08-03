// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestBedrockProviderCapabilities(t *testing.T) {
	caps := bedrockProviderCapabilities()
	if !caps.Task {
		t.Fatal("Task capability must be true")
	}
	if caps.Session || caps.Resume || caps.Rewind || caps.Cost || caps.Permissions || caps.TmuxAttach || caps.TerminalBytes {
		t.Fatalf("v1 must claim Task only, got %+v", caps)
	}
	if got := (bedrockTaskBackend{}).Capabilities(); got != caps {
		t.Fatalf("backend caps = %+v, want %+v", got, caps)
	}
}

func TestTaskBackendForProviderBedrock(t *testing.T) {
	b, ok := taskBackendForProvider(ProviderBedrock).(bedrockTaskBackend)
	if !ok {
		t.Fatalf("backend = %T, want bedrockTaskBackend", taskBackendForProvider(ProviderBedrock))
	}
	if !b.Capabilities().Task {
		t.Fatal("expected Task capability")
	}
}

func TestResolveBedrockSettings(t *testing.T) {
	t.Run("model from config preferred", func(t *testing.T) {
		env := map[string]string{
			bedrockRegionEnv: "us-west-2",
			bedrockModelEnv:  "env-model",
		}
		got, err := resolveBedrockSettings(mapGetenv(env), "cfg-model")
		if err != nil {
			t.Fatal(err)
		}
		if got.ModelID != "cfg-model" || got.Region != "us-west-2" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("model from env when config empty", func(t *testing.T) {
		env := map[string]string{
			awsRegionEnv:    "eu-west-1",
			bedrockModelEnv: "env-model",
			awsProfileEnv:   "work",
		}
		got, err := resolveBedrockSettings(mapGetenv(env), "")
		if err != nil {
			t.Fatal(err)
		}
		if got.ModelID != "env-model" || got.Region != "eu-west-1" || got.Profile != "work" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("region fallbacks", func(t *testing.T) {
		env := map[string]string{
			awsDefaultRegion: "ap-southeast-2",
			bedrockModelEnv:  "m",
		}
		got, err := resolveBedrockSettings(mapGetenv(env), "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Region != "ap-southeast-2" {
			t.Fatalf("region = %q", got.Region)
		}
	})
	t.Run("CLAUDIA region wins over AWS_REGION", func(t *testing.T) {
		env := map[string]string{
			bedrockRegionEnv: "us-east-1",
			awsRegionEnv:     "us-west-2",
			bedrockModelEnv:  "m",
		}
		got, err := resolveBedrockSettings(mapGetenv(env), "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Region != "us-east-1" {
			t.Fatalf("region = %q", got.Region)
		}
	})
	t.Run("missing model", func(t *testing.T) {
		_, err := resolveBedrockSettings(mapGetenv(map[string]string{bedrockRegionEnv: "us-east-1"}), "")
		if err == nil || !strings.Contains(err.Error(), "model") {
			t.Fatalf("err = %v, want model required", err)
		}
	})
	t.Run("missing region", func(t *testing.T) {
		_, err := resolveBedrockSettings(mapGetenv(map[string]string{bedrockModelEnv: "m"}), "")
		if err == nil || !strings.Contains(err.Error(), "region") {
			t.Fatalf("err = %v, want region required", err)
		}
	})
}

func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestBuildBedrockConverseInput(t *testing.T) {
	in := buildBedrockConverseInput("anthropic.claude-test", "hello")
	if in.ModelId == nil || *in.ModelId != "anthropic.claude-test" {
		t.Fatalf("ModelId = %v", in.ModelId)
	}
	if len(in.Messages) != 1 || in.Messages[0].Role != types.ConversationRoleUser {
		t.Fatalf("messages = %#v", in.Messages)
	}
	if len(in.Messages[0].Content) != 1 {
		t.Fatalf("content len = %d", len(in.Messages[0].Content))
	}
	tb, ok := in.Messages[0].Content[0].(*types.ContentBlockMemberText)
	if !ok || tb.Value != "hello" {
		t.Fatalf("content = %#v", in.Messages[0].Content[0])
	}
}

func TestMapBedrockTextDeltaAndUsage(t *testing.T) {
	if mapBedrockTextDelta("") != nil {
		t.Fatal("empty delta should be nil")
	}
	ev := mapBedrockTextDelta("hi")
	if ev == nil || ev.Type != TaskEventText || ev.Content != "hi" {
		t.Fatalf("ev = %#v", ev)
	}

	u := mapBedrockUsage(&types.TokenUsage{
		InputTokens:           aws.Int32(10),
		OutputTokens:          aws.Int32(20),
		CacheReadInputTokens:  aws.Int32(3),
		CacheWriteInputTokens: aws.Int32(4),
	})
	if u.InputTokens != 10 || u.OutputTokens != 20 || u.CacheReadInputTokens != 3 || u.CacheCreationInputTokens != 4 {
		t.Fatalf("usage = %+v", u)
	}
	if mapBedrockUsage(nil) != (Usage{}) {
		t.Fatal("nil usage should be zero")
	}
}

func TestMapBedrockAPIError(t *testing.T) {
	cases := []struct {
		in, wantSub string
	}{
		{"no credential providers", "credentials unavailable"},
		{"AccessDeniedException", "access denied"},
		{"ValidationException: bad model", "validation"},
		{"throttling", "bedrock: throttling"},
	}
	for _, tc := range cases {
		got := mapBedrockAPIError(errors.New(tc.in))
		if !strings.Contains(strings.ToLower(got), strings.ToLower(tc.wantSub)) {
			t.Errorf("map(%q) = %q, want substring %q", tc.in, got, tc.wantSub)
		}
	}
	if mapBedrockAPIError(nil) != "" {
		t.Fatal("nil err should map to empty")
	}
}

// fakeBedrockStreamer is a hermetic ConverseStream stand-in.
type fakeBedrockStreamer struct {
	mu       sync.Mutex
	lastArgs bedrockStreamArgs
	events   []TaskEvent
	err      error
	// if delay > 0, wait before closing (for cancel tests)
	delay time.Duration
	// block until ctx cancelled when hold is true
	hold bool
}

func (f *fakeBedrockStreamer) Stream(ctx context.Context, args bedrockStreamArgs) (<-chan TaskEvent, error) {
	f.mu.Lock()
	f.lastArgs = args
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan TaskEvent, len(f.events)+1)
	go func() {
		defer close(ch)
		for _, ev := range f.events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
		if f.hold {
			<-ctx.Done()
			return
		}
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}

func TestHermeticBedrockTaskRunStream(t *testing.T) {
	fake := &fakeBedrockStreamer{
		events: []TaskEvent{
			{Type: TaskEventText, Content: "Hel"},
			{Type: TaskEventText, Content: "lo"},
			{Type: TaskEventResult, Content: "Hello", Usage: Usage{InputTokens: 1, OutputTokens: 2}},
		},
	}
	// Env for settings validation; streamer is injected so no AWS call.
	t.Setenv(bedrockRegionEnv, "us-east-1")
	task := newTaskWithBackend(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-h1",
		Model:    "anthropic.claude-test",
	}, bedrockTaskBackend{streamer: fake})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := task.Run(ctx, "Say hello")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	var result *TaskEvent
	for ev := range ch {
		switch ev.Type {
		case TaskEventText:
			texts = append(texts, ev.Content)
		case TaskEventResult:
			cp := ev
			result = &cp
		case TaskEventError:
			t.Fatalf("unexpected error event: %+v", ev)
		}
	}
	if strings.Join(texts, "") != "Hello" {
		t.Fatalf("texts = %v", texts)
	}
	if result == nil || result.Content != "Hello" || result.Usage.OutputTokens != 2 {
		t.Fatalf("result = %#v", result)
	}
	if task.LastResult() != "Hello" {
		t.Fatalf("LastResult = %q", task.LastResult())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.lastArgs.Prompt != "Say hello" || fake.lastArgs.ModelID != "anthropic.claude-test" {
		t.Fatalf("lastArgs = %+v", fake.lastArgs)
	}
}

func TestHermeticBedrockTaskMissingModel(t *testing.T) {
	t.Setenv(bedrockRegionEnv, "us-east-1")
	t.Setenv(bedrockModelEnv, "")
	// Clear might not clear process env if parent set it; use empty config and empty env.
	task := newTaskWithBackend(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-no-model",
	}, bedrockTaskBackend{streamer: &fakeBedrockStreamer{}})
	_, err := task.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("err = %v, want model required", err)
	}
}

func TestHermeticBedrockTaskMissingRegion(t *testing.T) {
	t.Setenv(bedrockRegionEnv, "")
	t.Setenv(awsRegionEnv, "")
	t.Setenv(awsDefaultRegion, "")
	task := newTaskWithBackend(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-no-region",
		Model:    "m",
	}, bedrockTaskBackend{streamer: &fakeBedrockStreamer{}})
	_, err := task.Run(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("err = %v, want region required", err)
	}
}

func TestHermeticBedrockTaskStreamerError(t *testing.T) {
	t.Setenv(bedrockRegionEnv, "us-east-1")
	fake := &fakeBedrockStreamer{err: errors.New("no credential providers in chain")}
	task := newTaskWithBackend(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-auth",
		Model:    "m",
	}, bedrockTaskBackend{streamer: fake})
	_, err := task.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("want error")
	}
	// Streamer error is returned as-is from Stream; RunTask does not re-wrap fake errs.
	if !strings.Contains(err.Error(), "credential") {
		t.Fatalf("err = %v", err)
	}
}

func TestHermeticBedrockTaskErrorEvent(t *testing.T) {
	t.Setenv(bedrockRegionEnv, "us-east-1")
	fake := &fakeBedrockStreamer{
		events: []TaskEvent{
			{Type: TaskEventError, IsError: true, ErrorMsg: "bedrock: access denied"},
		},
	}
	task := newTaskWithBackend(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-err-ev",
		Model:    "m",
	}, bedrockTaskBackend{streamer: fake})
	ch, err := task.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for ev := range ch {
		if ev.Type == TaskEventError && ev.IsError {
			saw = true
		}
	}
	if !saw {
		t.Fatal("expected error event")
	}
	if !strings.Contains(task.LastResult(), "access denied") {
		t.Fatalf("LastResult = %q", task.LastResult())
	}
}

func TestHermeticBedrockTaskCancel(t *testing.T) {
	t.Setenv(bedrockRegionEnv, "us-east-1")
	fake := &fakeBedrockStreamer{hold: true}
	task := newTaskWithBackend(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-cancel",
		Model:    "m",
	}, bedrockTaskBackend{streamer: fake})
	ch, err := task.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Cancel(); err != nil {
		t.Fatal(err)
	}
	// Channel should close promptly after cancel.
	select {
	case _, ok := <-ch:
		if ok {
			// drain
			for range ch {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events channel did not close after Cancel")
	}
}

func TestStartBedrockSessionFailsClosed(t *testing.T) {
	_, err := Start(Config{Provider: ProviderBedrock, WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("want CapabilityError")
	}
	capErr, ok := err.(*CapabilityError)
	if !ok {
		t.Fatalf("err = %T %v, want *CapabilityError", err, err)
	}
	if capErr.Provider != ProviderBedrock || capErr.Capability != "session" || capErr.Status != CapabilityUnsupported {
		t.Fatalf("CapabilityError = %+v", capErr)
	}
}

func TestBedrockRewindFailsClosed(t *testing.T) {
	agent := &Agent{provider: ProviderBedrock}
	_, err := agent.Rewind(1, Config{Provider: ProviderBedrock})
	capErr, ok := err.(*CapabilityError)
	if !ok {
		t.Fatalf("err = %T %v", err, err)
	}
	if capErr.Capability != "rewind" || capErr.Status != CapabilityUnsupported {
		t.Fatalf("CapabilityError = %+v", capErr)
	}
}

func TestNewTaskProviderBedrock(t *testing.T) {
	task := NewTask(TaskConfig{Provider: ProviderBedrock, ID: "x", Model: "m"})
	if _, ok := task.backend.(bedrockTaskBackend); !ok {
		t.Fatalf("backend = %T", task.backend)
	}
}

// bedrockTaskSuccessOracle asserts the streamed text path claimed for v1.
func bedrockTaskSuccessOracle(events []TaskEvent) error {
	var text strings.Builder
	var sawResult bool
	for _, ev := range events {
		switch ev.Type {
		case TaskEventText:
			text.WriteString(ev.Content)
		case TaskEventResult:
			sawResult = true
			if ev.Content != text.String() {
				return errors.New("result content must equal concatenated text deltas")
			}
		case TaskEventError:
			return errors.New("unexpected error event")
		case TaskEventToolUse:
			return errors.New("v1 does not claim tool_use")
		}
	}
	if !sawResult {
		return errors.New("missing result event")
	}
	if text.Len() == 0 {
		return errors.New("empty assistant text")
	}
	return nil
}

func TestBedrockTaskSuccessOracle(t *testing.T) {
	good := []TaskEvent{
		{Type: TaskEventText, Content: "a"},
		{Type: TaskEventText, Content: "b"},
		{Type: TaskEventResult, Content: "ab"},
	}
	if err := bedrockTaskSuccessOracle(good); err != nil {
		t.Fatal(err)
	}
	if err := bedrockTaskSuccessOracle([]TaskEvent{{Type: TaskEventText, Content: "a"}}); err == nil {
		t.Fatal("want missing result")
	}
	if err := bedrockTaskSuccessOracle([]TaskEvent{
		{Type: TaskEventText, Content: "a"},
		{Type: TaskEventResult, Content: "wrong"},
	}); err == nil {
		t.Fatal("want content mismatch")
	}
	if err := bedrockTaskSuccessOracle([]TaskEvent{
		{Type: TaskEventToolUse, ToolName: "Bash"},
		{Type: TaskEventResult, Content: ""},
	}); err == nil {
		t.Fatal("want tool_use reject")
	}
}

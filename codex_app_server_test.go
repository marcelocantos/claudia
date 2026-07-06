// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeCodexAppServerTestTimeout       = 2 * time.Second
	fakeCodexAppServerSubscriberMinimum = 2
)

func TestParseCodexAppServerSuccessFixture(t *testing.T) {
	var threadID, turnID, command, finalText string
	var usage Usage
	var completed bool
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/success.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if !ok {
			continue
		}
		if ev.ThreadID != "" {
			threadID = ev.ThreadID
		}
		if ev.TurnID != "" {
			turnID = ev.TurnID
		}
		if ev.ItemType == "command_execution" {
			command = ev.Command
		}
		if ev.ItemType == "agent_message" {
			finalText = ev.Text
		}
		if ev.Method == "turn/completed" {
			completed = true
			usage = ev.Usage
		}
	}
	if threadID != "thr_success" {
		t.Fatalf("threadID = %q, want thr_success", threadID)
	}
	if turnID != "turn_success" {
		t.Fatalf("turnID = %q, want turn_success", turnID)
	}
	if command != "bash -lc ls" {
		t.Fatalf("command = %q, want bash -lc ls", command)
	}
	if finalText != "Final answer." {
		t.Fatalf("finalText = %q, want Final answer.", finalText)
	}
	if !completed {
		t.Fatal("success fixture did not produce turn/completed")
	}
	if usage.InputTokens != 10 || usage.CacheReadInputTokens != 4 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestParseCodexAppServerFailureFixture(t *testing.T) {
	var failed bool
	var errorMsg string
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/failure.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if ok && ev.Method == "turn/completed" {
			failed = ev.Status == "failed"
			errorMsg = ev.ErrorMsg
		}
	}
	if !failed {
		t.Fatal("failure fixture did not produce failed turn/completed")
	}
	if errorMsg != "model failed" {
		t.Fatalf("errorMsg = %q, want model failed", errorMsg)
	}
}

func TestParseCodexAppServerInterruptedFixture(t *testing.T) {
	var interrupted bool
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/interrupted.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if ok && ev.Method == "turn/completed" {
			interrupted = ev.Status == "interrupted"
		}
	}
	if !interrupted {
		t.Fatal("interrupted fixture did not produce interrupted turn/completed")
	}
}

func TestParseCodexAppServerUnsupportedCapabilityFixture(t *testing.T) {
	line := readFixtureLines(t, "testdata/codex/app-server/unsupported-capability.jsonl")[0]
	ev, ok, err := parseCodexAppServerLine([]byte(line))
	if err != nil {
		t.Fatalf("parseCodexAppServerLine: %v", err)
	}
	if !ok {
		t.Fatal("unsupported-capability fixture was ignored")
	}
	if !ev.IsError || !ev.IsResponse {
		t.Fatalf("event = %+v, want response error", ev)
	}
	if ev.ErrorMsg != "requires experimentalApi capability" {
		t.Fatalf("ErrorMsg = %q, want requires experimentalApi capability", ev.ErrorMsg)
	}
}

func TestParseCodexAppServerMalformedLine(t *testing.T) {
	if _, _, err := parseCodexAppServerLine([]byte(`{"method":`)); err == nil {
		t.Fatal("malformed line returned nil error")
	}
}

type fakeCodexAppServerBackend struct {
	fixture     string
	allowEvents chan struct{}

	mu         sync.Mutex
	requests   []agentStartRequest
	sends      []string
	interrupts int
	stops      int
	errors     []error
}

func newFakeCodexAppServerBackend(fixture string) *fakeCodexAppServerBackend {
	return &fakeCodexAppServerBackend{
		fixture:     fixture,
		allowEvents: make(chan struct{}),
	}
}

func (b *fakeCodexAppServerBackend) Capabilities() providerCapabilities {
	return providerCapabilities{
		Session: true,
		Resume:  true,
	}
}

func (b *fakeCodexAppServerBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()
	return &agentStart{
		WindowID:  "codex-app-server-" + req.SessionID,
		Control:   nil,
		Ops:       b.ops(),
		TailJSONL: false,
		DetectReady: func(a *Agent) {
			close(a.ready)
		},
	}, nil
}

func (b *fakeCodexAppServerBackend) ops() agentOps {
	return agentOps{
		interrupt: func(a *Agent) error {
			b.mu.Lock()
			b.interrupts++
			b.mu.Unlock()
			return b.publishFixture(a, "testdata/codex/app-server/interrupted.jsonl")
		},
		send: func(a *Agent, msg string) error {
			b.mu.Lock()
			b.sends = append(b.sends, msg)
			b.mu.Unlock()
			go func() {
				<-b.allowEvents
				if err := b.publishFixture(a, b.fixture); err != nil {
					b.mu.Lock()
					b.errors = append(b.errors, err)
					b.mu.Unlock()
				}
			}()
			return nil
		},
		stop: func(*Agent) {
			b.mu.Lock()
			b.stops++
			b.mu.Unlock()
		},
	}
}

func (b *fakeCodexAppServerBackend) publishFixture(a *Agent, path string) error {
	lines, err := appServerFixtureLines(path)
	if err != nil {
		return err
	}
	for _, line := range lines {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if agentEv, ok := ev.agentEvent(); ok {
			a.publishEvent(agentEv)
		}
	}
	return nil
}

func appServerFixtureLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func (b *fakeCodexAppServerBackend) request(t *testing.T) agentStartRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(b.requests))
	}
	return b.requests[0]
}

func TestFakeCodexAppServerLifecycle(t *testing.T) {
	backend := newFakeCodexAppServerBackend("testdata/codex/app-server/success.jsonl")
	agent, err := startWithBackend(Config{
		Provider:    ProviderCodex,
		WorkDir:     t.TempDir(),
		SessionID:   "thr-success",
		Model:       "gpt-5-codex",
		TermLogPath: "-",
	}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	req := backend.request(t)
	if req.Config.Model != "gpt-5-codex" || req.SessionID != "thr-success" {
		t.Fatalf("request = %+v", req)
	}
	if got := agent.AttachCommand(); got != "" {
		t.Fatalf("AttachCommand = %q, want empty for app-server fake", got)
	}
	if got := agent.TermLogPath(); got != "" {
		t.Fatalf("TermLogPath = %q, want disabled", got)
	}

	var (
		mu       sync.Mutex
		progress int
		rawLines int
	)
	token := agent.SubscribeEvents(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		if ev.ProgressType == "tool_use" {
			progress++
		}
		if len(ev.Raw) > 0 {
			rawLines++
		}
	})
	defer agent.UnsubscribeEvents(token)

	ctx, cancel := context.WithTimeout(context.Background(), fakeCodexAppServerTestTimeout)
	defer cancel()
	replyCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		reply, err := agent.WaitForResponse(ctx)
		if err != nil {
			errCh <- err
			return
		}
		replyCh <- reply
	}()
	waitForEventSubscribers(t, agent, fakeCodexAppServerSubscriberMinimum)
	if err := agent.Send("hello codex"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	close(backend.allowEvents)

	select {
	case err := <-errCh:
		t.Fatalf("WaitForResponse: %v", err)
	case reply := <-replyCh:
		if reply != "Final answer." {
			t.Fatalf("reply = %q, want Final answer.", reply)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	usage := agent.Usage()
	if usage.InputTokens != 10 || usage.CacheReadInputTokens != 4 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
	mu.Lock()
	gotProgress := progress
	gotRawLines := rawLines
	mu.Unlock()
	if gotProgress == 0 {
		t.Fatal("expected a progress/tool event")
	}
	if gotRawLines == 0 {
		t.Fatal("expected raw app-server payloads on provider-neutral events")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.sends) != 1 || backend.sends[0] != "hello codex" {
		t.Fatalf("sends = %v", backend.sends)
	}
	if len(backend.errors) != 0 {
		t.Fatalf("fixture errors = %v", backend.errors)
	}
}

func waitForEventSubscribers(t *testing.T, agent *Agent, want int) {
	t.Helper()
	deadline := time.Now().Add(fakeCodexAppServerTestTimeout)
	for time.Now().Before(deadline) {
		agent.mu.Lock()
		got := len(agent.eventSubs)
		agent.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	agent.mu.Lock()
	got := len(agent.eventSubs)
	agent.mu.Unlock()
	t.Fatalf("event subscribers = %d, want at least %d", got, want)
}

func TestFakeCodexAppServerInterruptLifecycle(t *testing.T) {
	backend := newFakeCodexAppServerBackend("testdata/codex/app-server/success.jsonl")
	agent, err := startWithBackend(Config{
		Provider:    ProviderCodex,
		WorkDir:     t.TempDir(),
		SessionID:   "thr-interrupt",
		TermLogPath: "-",
	}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()

	var terminal bool
	token := agent.SubscribeEvents(func(ev Event) {
		if ev.IsTerminalStop() {
			terminal = true
		}
	})
	defer agent.UnsubscribeEvents(token)

	if err := agent.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if !terminal {
		t.Fatal("interrupt fixture did not publish terminal event")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.interrupts != 1 {
		t.Fatalf("interrupts = %d, want 1", backend.interrupts)
	}
}

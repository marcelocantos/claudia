// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeCodexAppServerTestTimeout is deliberately absent: the 2s budget it
// named decided verdicts by wall clock, and 🎯T31 replaced every use with a
// signal (the subscriber count, the reply channel) or with `go test -timeout`.
const fakeCodexAppServerSubscriberMinimum = 2

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

func TestCodexAppServerEventsCarryTrackedTurnIdentity(t *testing.T) {
	var got []Event
	c := &codexAppServerClient{
		threadID: "thr-identity",
		onEvent:  func(ev Event) { got = append(got, ev) },
	}
	startTurn := func(turnID, itemID, text string, responseID int) {
		t.Helper()
		c.mu.Lock()
		c.inFlight = true
		c.turnID = ""
		c.mu.Unlock()
		c.dispatch([]byte(fmt.Sprintf(`{"id":%d,"result":{"turn":{"id":%q,"status":"inProgress"}}}`, responseID, turnID)))
		// Deliberately omit turnId: dispatch must stamp the turn/start identity
		// it already tracks, not leave consumers to infer it.
		c.dispatch([]byte(fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"thr-identity","item":{"id":%q,"type":"agent_message","text":%q}}}`, itemID, text)))
		c.dispatch([]byte(fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"thr-identity","turn":{"id":%q,"status":"completed"}}}`, turnID)))
	}

	startTurn("turn-1", "item-1", "first", 1)
	// A backend item outside an in-flight turn must not inherit turn-1.
	c.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"thr-identity","item":{"id":"item-outside","type":"agent_message","text":"outside"}}}`))
	startTurn("turn-2", "item-2", "second", 2)

	if len(got) != 5 {
		t.Fatalf("events = %d, want 5: %+v", len(got), got)
	}
	checks := []struct {
		index     int
		turnID    string
		messageID string
		terminal  bool
	}{
		{0, "turn-1", "item-1", false},
		{1, "turn-1", "", true},
		{2, "", "item-outside", false},
		{3, "turn-2", "item-2", false},
		{4, "turn-2", "", true},
	}
	for _, check := range checks {
		ev := got[check.index]
		if ev.SessionID != "thr-identity" || ev.TurnID != check.turnID || ev.MessageID != check.messageID {
			t.Errorf("event[%d] identity = session %q turn %q message %q, want thr-identity/%q/%q",
				check.index, ev.SessionID, ev.TurnID, ev.MessageID, check.turnID, check.messageID)
		}
		if ev.IsTerminalStop() != check.terminal {
			t.Errorf("event[%d] terminal = %v, want %v", check.index, ev.IsTerminalStop(), check.terminal)
		}
	}
	if got[0].TurnID == got[3].TurnID {
		t.Fatalf("consecutive Codex turns share TurnID %q", got[0].TurnID)
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

func TestParseCodexAppServerLiveTurnFixture(t *testing.T) {
	// Redacted live capture against codex-cli 0.146.0-alpha.9.2 (🎯T4.4).
	// Proves camelCase item types, notification order, and tokenUsage path.
	var (
		threadID, turnID, model, approval, sandbox, finalText string
		usage                                                 Usage
		methods                                               []string
		sawTurnCompleted                                      bool
	)
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/live-turn.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if !ok {
			continue
		}
		if ev.Method != "" {
			methods = append(methods, ev.Method)
		}
		if ev.Method == "thread/start" {
			threadID = ev.ThreadID
			model = ev.Model
			approval = ev.ApprovalPolicy
			sandbox = ev.SandboxType
		}
		if ev.Method == "turn/start" {
			turnID = ev.TurnID
		}
		if ev.ItemType == "agent_message" && ev.Text != "" {
			finalText = ev.Text
		}
		if ev.Method == "thread/tokenUsage/updated" {
			usage = ev.Usage
		}
		if ev.Method == "turn/completed" {
			sawTurnCompleted = ev.Status == "completed"
		}
		if agentEv, ok := ev.agentEvent(); ok && agentEv.Type == "assistant" && agentEv.Text == "T4.4-ok" {
			finalText = agentEv.Text
		}
	}
	if threadID != "thr_live" {
		t.Fatalf("threadID = %q, want thr_live", threadID)
	}
	if turnID != "turn_live" {
		t.Fatalf("turnID = %q, want turn_live", turnID)
	}
	if model != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", model)
	}
	if approval != "never" {
		t.Fatalf("approvalPolicy = %q, want never", approval)
	}
	if sandbox != "readOnly" {
		t.Fatalf("sandbox.type = %q, want readOnly (request sandbox=read-only)", sandbox)
	}
	if finalText != "T4.4-ok" {
		t.Fatalf("finalText = %q, want T4.4-ok", finalText)
	}
	if !sawTurnCompleted {
		t.Fatal("live fixture missing completed turn/completed")
	}
	if usage.InputTokens == 0 || usage.OutputTokens == 0 {
		t.Fatalf("tokenUsage not mapped from thread/tokenUsage/updated: %+v", usage)
	}
	// Notification order: thread/started before turn/started before item/*
	// before tokenUsage before turn/completed.
	wantOrder := []string{
		"thread/start",
		"thread/started",
		"turn/start",
		"turn/started",
		"item/started",
		"item/completed",
		"thread/tokenUsage/updated",
		"turn/completed",
	}
	pos := 0
	for _, m := range methods {
		if pos < len(wantOrder) && m == wantOrder[pos] {
			pos++
		}
	}
	if pos != len(wantOrder) {
		t.Fatalf("notification order incomplete: matched %d/%d of %v; got %v", pos, len(wantOrder), wantOrder, methods)
	}
}

func TestParseCodexAppServerLifecycleFixture(t *testing.T) {
	// Resume/fork/archive/interrupt shapes from non-ephemeral live spike.
	var sawResume, sawFork, sawInterrupted, sawArchived, sawUnarchived bool
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/lifecycle.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !ok {
			continue
		}
		if ev.Method == "thread/start" && ev.ThreadID == "thr_lifecycle" && ev.Model == "gpt-5.4" {
			sawResume = true
			if ev.ApprovalPolicy != "never" || ev.SandboxType != "readOnly" {
				t.Fatalf("resume config: approval=%q sandbox=%q", ev.ApprovalPolicy, ev.SandboxType)
			}
		}
		if ev.Method == "thread/start" && ev.ThreadID == "thr_fork" {
			sawFork = true
		}
		if ev.Method == "turn/completed" && ev.Status == "interrupted" {
			sawInterrupted = true
		}
		if ev.Method == "thread/archived" {
			sawArchived = true
		}
		if ev.Method == "thread/unarchived" {
			sawUnarchived = true
		}
	}
	if !sawResume || !sawFork || !sawInterrupted || !sawArchived || !sawUnarchived {
		t.Fatalf("lifecycle fixture incomplete: resume=%v fork=%v interrupt=%v archive=%v unarchive=%v",
			sawResume, sawFork, sawInterrupted, sawArchived, sawUnarchived)
	}
}

func TestCodexAppServerRequestBuilders(t *testing.T) {
	// Public method field names for Session mode — machine-checked against
	// the spike contract (cwd, model, approvalPolicy, sandbox, resume/fork/archive).
	initReq := codexAppServerInitialize(0, codexAppServerClientInfo{
		Name: "claudia", Title: "claudia", Version: "0.1.0",
	})
	if initReq.Method != "initialize" || initReq.ID == nil || *initReq.ID != 0 {
		t.Fatalf("initialize = %+v", initReq)
	}
	if codexAppServerInitialized().Method != "initialized" {
		t.Fatal("initialized method")
	}
	ephemeral := true
	start := codexAppServerThreadStart(1, codexAppServerThreadStartParams{
		CWD:            "/tmp/w",
		Model:          "gpt-5.4",
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
		Ephemeral:      &ephemeral,
	})
	raw, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"method":"thread/start"`, `"cwd":"/tmp/w"`, `"model":"gpt-5.4"`, `"approvalPolicy":"never"`, `"sandbox":"read-only"`, `"ephemeral":true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("thread/start missing %s in %s", want, raw)
		}
	}
	turn := codexAppServerTurnStart(2, codexAppServerTurnStartParams{
		ThreadID: "thr_x",
		Input:    []codexAppServerUserInput{{Type: "text", Text: "hi"}},
	})
	raw, err = json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"method":"turn/start"`, `"threadId":"thr_x"`, `"type":"text"`, `"text":"hi"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("turn/start missing %s in %s", want, raw)
		}
	}
	intr := codexAppServerTurnInterrupt(3, "thr_x", "turn_y")
	raw, _ = json.Marshal(intr)
	if !strings.Contains(string(raw), `"method":"turn/interrupt"`) || !strings.Contains(string(raw), `"turnId":"turn_y"`) {
		t.Fatalf("interrupt = %s", raw)
	}
	for _, tc := range []struct {
		name string
		req  codexAppServerRequest
		want string
	}{
		{"resume", codexAppServerThreadResume(4, codexAppServerThreadIDParams{ThreadID: "thr_x"}), "thread/resume"},
		{"fork", codexAppServerThreadFork(5, codexAppServerThreadIDParams{ThreadID: "thr_x"}), "thread/fork"},
		{"archive", codexAppServerThreadArchive(6, "thr_x"), "thread/archive"},
		{"unarchive", codexAppServerThreadUnarchive(7, "thr_x"), "thread/unarchive"},
	} {
		raw, _ := json.Marshal(tc.req)
		if !strings.Contains(string(raw), `"method":"`+tc.want+`"`) || !strings.Contains(string(raw), `"threadId":"thr_x"`) {
			t.Fatalf("%s = %s", tc.name, raw)
		}
	}
}

func TestNormalizeCodexAppServerItemType(t *testing.T) {
	cases := map[string]string{
		"agentMessage":      "agent_message",
		"commandExecution":  "command_execution",
		"userMessage":       "user_message",
		"agent_message":     "agent_message",
		"command_execution": "command_execution",
		"reasoning":         "reasoning",
	}
	for in, want := range cases {
		if got := normalizeCodexAppServerItemType(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodexAppServerAgentMessageStartedGuard(t *testing.T) {
	// item/started for an agentMessage arrives with empty text before the
	// deltas land; emitting it as an assistant event would let WaitForResponse
	// settle on an empty turn. Only started items that already carry text, and
	// item/completed, may produce assistant events.
	cases := []struct {
		name        string
		line        string
		wantEmitted bool
		wantText    string
	}{
		{
			name: "started with empty text emits nothing",
			line: `{"method":"item/started","params":{"item":{"type":"agentMessage","id":"item_msg","text":"","phase":"final_answer"},"threadId":"thr_guard","turnId":"turn_guard"}}`,
		},
		{
			name:        "started with text still emits assistant",
			line:        `{"method":"item/started","params":{"item":{"type":"agentMessage","id":"item_msg","text":"partial answer","phase":"final_answer"},"threadId":"thr_guard","turnId":"turn_guard"}}`,
			wantEmitted: true,
			wantText:    "partial answer",
		},
		{
			name:        "completed with empty text still emits assistant",
			line:        `{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"item_msg","text":"","phase":"final_answer"},"threadId":"thr_guard","turnId":"turn_guard"}}`,
			wantEmitted: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok, err := parseCodexAppServerLine([]byte(tc.line))
			if err != nil {
				t.Fatalf("parseCodexAppServerLine(%q): %v", tc.line, err)
			}
			if !ok {
				t.Fatalf("parseCodexAppServerLine(%q) yielded no event", tc.line)
			}
			agentEv, emitted := ev.agentEvent()
			if emitted != tc.wantEmitted {
				t.Fatalf("agentEvent() emitted = %v (type %q, text %q), want %v", emitted, agentEv.Type, agentEv.Text, tc.wantEmitted)
			}
			if !emitted {
				return
			}
			if agentEv.Type != "assistant" {
				t.Fatalf("agentEvent().Type = %q, want assistant", agentEv.Type)
			}
			if agentEv.Text != tc.wantText {
				t.Fatalf("agentEvent().Text = %q, want %q", agentEv.Text, tc.wantText)
			}
		})
	}
}

func TestCodexAppServerLiveFixtureNoEmptyAssistant(t *testing.T) {
	// The live capture contains the real empty-text item/started agentMessage;
	// no agent_message line in it may yield an empty assistant event.
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/live-turn.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if !ok || ev.ItemType != "agent_message" {
			continue
		}
		if agentEv, emitted := ev.agentEvent(); emitted && agentEv.Type == "assistant" && agentEv.Text == "" {
			t.Fatalf("empty assistant event from %s method %s", line, ev.Method)
		}
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

	// t.Context(): cleanup, not a verdict clock (🎯T31). The fake answers as
	// fast as the scheduler allows; a busy machine must make this slower, not
	// RED.
	ctx := t.Context()
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

	// No timeout arm: WaitForResponse either answers or errors. A hang is a
	// real defect and belongs to `go test -timeout`, which names the stuck
	// goroutine — a 2s arm only named this select (🎯T31).
	select {
	case err := <-errCh:
		t.Fatalf("WaitForResponse: %v", err)
	case reply := <-replyCh:
		if reply != "Final answer." {
			t.Fatalf("reply = %q, want Final answer.", reply)
		}
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

// waitForEventSubscribers blocks until at least want subscribers are
// installed. The deadline it used to carry decided the verdict by wall clock:
// a loaded machine that had not yet scheduled the WaitForResponse goroutine
// drew RED for a product that was fine (🎯T31). A slow machine now makes this
// slower, never RED; a subscriber that is never installed hangs until
// `go test -timeout` names the goroutine that failed to run.
func waitForEventSubscribers(t *testing.T, agent *Agent, want int) {
	t.Helper()
	waitForCond(t, fmt.Sprintf("%d event subscriber(s) installed", want), func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return len(agent.eventSubs) >= want
	})
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

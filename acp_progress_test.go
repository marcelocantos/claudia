// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"io"
	"strconv"
	"testing"
)

func TestT50_1ThoughtPlanThenToolThenAssistantInOrder(t *testing.T) {
	thought := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"considering"}}}`)
	plan := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"plan","entries":[{"content":"step one"}]}}`)
	tool := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"tc1","title":"Read","status":"in_progress"}}`)
	asst := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}`)
	seq := []json.RawMessage{thought, plan, tool, asst}
	wantType := []string{"progress", "progress", "progress", "assistant"}
	wantProg := []string{ProgressThought, ProgressPlan, ProgressToolUse, ""}

	for _, cl := range []struct {
		name string
		feed func(on EventFunc, params json.RawMessage)
	}{
		{"grok", func(on EventFunc, params json.RawMessage) {
			c := &grokACPClient{sessionID: "s1", prompts: acpPromptStack{ids: []int64{7}}, onEvent: on}
			c.handleSessionUpdate(params)
		}},
		{"cursor", func(on EventFunc, params json.RawMessage) {
			c := &cursorACPClient{sessionID: "s1", prompts: acpPromptStack{ids: []int64{7}}, onEvent: on}
			c.handleSessionUpdate(params)
		}},
	} {
		var got []Event
		on := func(ev Event) { got = append(got, ev) }
		for _, params := range seq {
			cl.feed(on, params)
		}
		if len(got) != 4 {
			t.Fatalf("%s events=%d want 4: %+v", cl.name, len(got), got)
		}
		for i := range got {
			if got[i].Type != wantType[i] || got[i].ProgressType != wantProg[i] {
				t.Fatalf("%s event[%d] type=%q progress=%q", cl.name, i, got[i].Type, got[i].ProgressType)
			}
			if got[i].SessionID != "s1" || got[i].TurnID != "7" {
				t.Fatalf("%s event[%d] identity %q/%q", cl.name, i, got[i].SessionID, got[i].TurnID)
			}
			if got[i].Type == "assistant" && got[i].PreviewUpdate != PreviewUpdateAppend {
				t.Fatalf("%s assistant chunk PreviewUpdate=%q want %q", cl.name, got[i].PreviewUpdate, PreviewUpdateAppend)
			}
		}
		if got[0].Text != "considering" {
			t.Fatalf("%s thought text=%q", cl.name, got[0].Text)
		}
		if got[1].Text != "step one" {
			t.Fatalf("%s plan text=%q", cl.name, got[1].Text)
		}
		if got[0].Type == "assistant" || got[1].Type == "assistant" {
			t.Fatalf("%s thought/plan must not be assistant", cl.name)
		}
	}
}

func TestT50_2PromptAcceptedBeforeWriteAndPermissionBeforeReply(t *testing.T) {
	perm := json.RawMessage(`{"options":[{"optionId":"allow_always"}]}`)
	chunk := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`)

	t.Run("grok", func(t *testing.T) {
		var got []Event
		c := &grokACPClient{sessionID: "s1", onEvent: func(ev Event) { got = append(got, ev) }, stdin: discardWrite{}}
		if err := c.Prompt("hi"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if !c.promptInFlight() {
			t.Fatal("PromptInFlight false after dispatch")
		}
		c.mu.Lock()
		pid := c.prompts.top()
		c.mu.Unlock()
		id := int64(99)
		c.handleServerRequest(acpRPCMessage{ID: &id, Method: "session/request_permission", Params: perm})
		c.handleSessionUpdate(chunk)
		c.dispatchMessage([]byte(`{"jsonrpc":"2.0","id":` + strconv.FormatInt(pid, 10) + `,"result":{"stopReason":"end_turn"}}`))
		assertAcceptedPermAssistant(t, got)
		if c.promptInFlight() {
			t.Fatal("PromptInFlight still true after terminal")
		}
	})

	t.Run("cursor", func(t *testing.T) {
		var got []Event
		c := &cursorACPClient{sessionID: "s1", onEvent: func(ev Event) { got = append(got, ev) }, stdin: discardWrite{}}
		if err := c.Prompt("hi"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if !c.promptInFlight() {
			t.Fatal("PromptInFlight false after dispatch")
		}
		c.mu.Lock()
		pid := c.prompts.top()
		c.mu.Unlock()
		id := int64(99)
		c.handleServerRequest(acpRPCMessage{ID: &id, Method: "session/request_permission", Params: perm})
		c.handleSessionUpdate(chunk)
		c.dispatchMessage([]byte(`{"jsonrpc":"2.0","id":` + strconv.FormatInt(pid, 10) + `,"result":{"stopReason":"end_turn"}}`))
		assertAcceptedPermAssistant(t, got)
		if c.promptInFlight() {
			t.Fatal("PromptInFlight still true after terminal")
		}
	})
}

func assertAcceptedPermAssistant(t *testing.T, got []Event) {
	t.Helper()
	if len(got) < 4 {
		t.Fatalf("events=%d want ≥4 (accepted, permission, assistant, terminal): %+v", len(got), got)
	}
	if got[0].ProgressType != ProgressPromptAccepted || got[0].TurnID == "" {
		t.Fatalf("first event = %+v, want prompt_accepted with TurnID", got[0])
	}
	if got[1].ProgressType != ProgressPermission {
		t.Fatalf("second event = %+v, want permission before later updates", got[1])
	}
	if got[2].Type != "assistant" {
		t.Fatalf("third event = %+v, want assistant update after accepted", got[2])
	}
	if !got[len(got)-1].IsTerminalStop() {
		t.Fatalf("last event = %+v, want terminal", got[len(got)-1])
	}
}

func TestT50_3ToolIdentityAndUsageWithoutParsingRaw(t *testing.T) {
	with := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call-9","title":"Bash","status":"pending","_meta":{"totalTokens":42}}}`)
	without := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call"}}`)
	asst := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ok"},"_meta":{"totalTokens":7}}}`)

	for _, name := range []string{"grok", "cursor"} {
		var got []Event
		on := func(ev Event) { got = append(got, ev) }
		feed := func(params json.RawMessage) {
			switch name {
			case "grok":
				(&grokACPClient{sessionID: "s1", prompts: acpPromptStack{ids: []int64{3}}, onEvent: on}).handleSessionUpdate(params)
			case "cursor":
				(&cursorACPClient{sessionID: "s1", prompts: acpPromptStack{ids: []int64{3}}, onEvent: on}).handleSessionUpdate(params)
			}
		}
		feed(with)
		feed(without)
		feed(asst)
		if len(got) != 3 {
			t.Fatalf("%s events=%d want 3", name, len(got))
		}
		if got[0].ToolCallID != "call-9" || got[0].ToolTitle != "Bash" || got[0].ToolStatus != "pending" {
			t.Fatalf("%s promoted fields = %+v", name, got[0])
		}
		if got[0].Usage.OutputTokens != 42 {
			t.Fatalf("%s usage = %+v, want 42 from _meta.totalTokens", name, got[0].Usage)
		}
		if got[1].ToolCallID != "" || got[1].ToolTitle != "" || got[1].ToolStatus != "" {
			t.Fatalf("%s empty fixture invented identity: %+v", name, got[1])
		}
		if got[2].Type != "assistant" || got[2].Usage.OutputTokens != 7 {
			t.Fatalf("%s assistant usage = %+v", name, got[2])
		}
	}
}

type discardWrite struct{}

func (discardWrite) Write(p []byte) (int, error) { return len(p), nil }
func (discardWrite) Close() error                { return nil }

var _ io.WriteCloser = discardWrite{}

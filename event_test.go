// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"testing"
)

func TestParseEventAssistantText(t *testing.T) {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "Hello"},
			},
			"stop_reason": "end_turn",
		},
	}
	line, _ := json.Marshal(msg)
	ev := parseEvent(string(line))

	if ev.Type != "assistant" {
		t.Errorf("Type = %q, want assistant", ev.Type)
	}
	if ev.Text != "Hello" {
		t.Errorf("Text = %q, want Hello", ev.Text)
	}
	if ev.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", ev.StopReason)
	}
	if !ev.IsTerminalStop() {
		t.Error("IsTerminalStop = false, want true for end_turn")
	}
}

func TestParseEventClaudeIdentity(t *testing.T) {
	line := `{"type":"assistant","sessionId":"sess-claude","uuid":"record-1","message":{"id":"msg-1","content":[{"type":"text","text":"hello"}]}}`
	ev := parseEvent(line)

	if ev.SessionID != "sess-claude" {
		t.Errorf("SessionID = %q, want sess-claude", ev.SessionID)
	}
	if ev.RecordID != "record-1" {
		t.Errorf("RecordID = %q, want record-1", ev.RecordID)
	}
	if ev.MessageID != "msg-1" {
		t.Errorf("MessageID = %q, want msg-1", ev.MessageID)
	}
	if ev.TurnID != "" {
		t.Errorf("TurnID = %q before transcript correlation, want empty", ev.TurnID)
	}
}

func TestClaudeEventCorrelatorUsesPromptRecordUUID(t *testing.T) {
	var correlator claudeEventCorrelator
	stamp := func(line string) Event {
		t.Helper()
		return correlator.correlate(parseEvent(line))
	}

	outsideBefore := stamp(`{"type":"system","sessionId":"sess-claude","uuid":"record-before"}`)
	if outsideBefore.TurnID != "" {
		t.Fatalf("event before prompt TurnID = %q, want empty", outsideBefore.TurnID)
	}

	prompt1 := stamp(`{"type":"user","sessionId":"sess-claude","uuid":"turn-1","message":{"content":"first prompt"}}`)
	delta1 := stamp(`{"type":"assistant","sessionId":"sess-claude","uuid":"record-1a","message":{"id":"msg-1","content":[{"type":"text","text":"partial"}]}}`)
	terminal1a := stamp(`{"type":"assistant","sessionId":"sess-claude","uuid":"record-1b","message":{"id":"msg-1","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"redacted"}]}}`)
	terminal1b := stamp(`{"type":"assistant","sessionId":"sess-claude","uuid":"record-1c","message":{"id":"msg-1","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`)
	for name, ev := range map[string]Event{
		"prompt":           prompt1,
		"delta":            delta1,
		"terminal-first":   terminal1a,
		"terminal-sibling": terminal1b,
	} {
		if ev.TurnID != "turn-1" {
			t.Errorf("%s TurnID = %q, want turn-1", name, ev.TurnID)
		}
	}
	if delta1.MessageID != "msg-1" || delta1.RecordID != "record-1a" {
		t.Fatalf("delta identity = message %q record %q", delta1.MessageID, delta1.RecordID)
	}

	outsideAfter := stamp(`{"type":"system","sessionId":"sess-claude","uuid":"record-between"}`)
	if outsideAfter.TurnID != "" {
		t.Fatalf("event after completed prompt TurnID = %q, want empty", outsideAfter.TurnID)
	}

	prompt2 := stamp(`{"type":"user","sessionId":"sess-claude","uuid":"turn-2","message":{"content":[{"type":"text","text":"second prompt"}]}}`)
	delta2 := stamp(`{"type":"assistant","sessionId":"sess-claude","uuid":"record-2a","message":{"id":"msg-2","content":[{"type":"text","text":"second"}]}}`)
	terminal2 := stamp(`{"type":"assistant","sessionId":"sess-claude","uuid":"record-2b","message":{"id":"msg-2","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`)
	for name, ev := range map[string]Event{"prompt": prompt2, "delta": delta2, "terminal": terminal2} {
		if ev.TurnID != "turn-2" {
			t.Errorf("second %s TurnID = %q, want turn-2", name, ev.TurnID)
		}
	}
	if delta1.TurnID == delta2.TurnID {
		t.Fatalf("consecutive prompts share TurnID %q", delta1.TurnID)
	}
}

func TestParseEventAssistantMultipleTextBlocks(t *testing.T) {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "first"},
				map[string]any{"type": "text", "text": "second"},
			},
		},
	}
	line, _ := json.Marshal(msg)
	ev := parseEvent(string(line))

	// Multiple text blocks in a single JSONL event are joined with \n.
	want := "first\nsecond"
	if ev.Text != want {
		t.Errorf("Text = %q, want %q", ev.Text, want)
	}
}

func TestParseEventAssistantToolUseNotTerminal(t *testing.T) {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash"},
			},
			"stop_reason": "tool_use",
		},
	}
	line, _ := json.Marshal(msg)
	ev := parseEvent(string(line))

	if ev.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", ev.StopReason)
	}
	if ev.IsTerminalStop() {
		t.Error("IsTerminalStop = true, want false for tool_use stop_reason")
	}
}

func TestParseEventAssistantStreamingChunk(t *testing.T) {
	// Intermediate streaming event: content block with no stop_reason yet.
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "partial"},
			},
		},
	}
	line, _ := json.Marshal(msg)
	ev := parseEvent(string(line))

	if ev.StopReason != "" {
		t.Errorf("StopReason = %q, want empty for streaming chunk", ev.StopReason)
	}
	if ev.IsTerminalStop() {
		t.Error("IsTerminalStop = true, want false for empty stop_reason")
	}
	if ev.Text != "partial" {
		t.Errorf("Text = %q, want partial", ev.Text)
	}
}

func TestIsTerminalStopForAllValues(t *testing.T) {
	cases := []struct {
		stopReason string
		want       bool
	}{
		{"end_turn", true},
		{"stop_sequence", true},
		{"max_tokens", true},
		{"tool_use", false},
		{"", false},
		{"unknown_value", false},
	}
	for _, tc := range cases {
		ev := Event{Type: "assistant", StopReason: tc.stopReason}
		if got := ev.IsTerminalStop(); got != tc.want {
			t.Errorf("IsTerminalStop(%q) = %v, want %v", tc.stopReason, got, tc.want)
		}
	}
}

func TestIsTerminalStopRequiresAssistantType(t *testing.T) {
	// Even with a terminal stop_reason, non-assistant events must not
	// be treated as turn-complete.
	ev := Event{Type: "user", StopReason: "end_turn"}
	if ev.IsTerminalStop() {
		t.Error("IsTerminalStop = true for user event, want false")
	}
	ev = Event{Type: "system", StopReason: "end_turn"}
	if ev.IsTerminalStop() {
		t.Error("IsTerminalStop = true for system event, want false")
	}
}

func TestParseEventProgress(t *testing.T) {
	line := `{"type":"progress","data":{"type":"tool_use"}}`
	ev := parseEvent(line)

	if ev.Type != "progress" {
		t.Errorf("Type = %q, want progress", ev.Type)
	}
	if ev.ProgressType != "tool_use" {
		t.Errorf("ProgressType = %q, want tool_use", ev.ProgressType)
	}
}

func TestParseEventSystem(t *testing.T) {
	// System events still parse — we just no longer use them as a
	// turn-complete signal.
	line := `{"type":"system","subtype":"stop_hook_summary"}`
	ev := parseEvent(line)

	if ev.Type != "system" {
		t.Errorf("Type = %q, want system", ev.Type)
	}
	if ev.IsTerminalStop() {
		t.Error("IsTerminalStop = true for system event")
	}
}

func TestParseEventInvalidJSON(t *testing.T) {
	ev := parseEvent("not json")
	if ev.Type != "unknown" {
		t.Errorf("Type = %q, want unknown", ev.Type)
	}
}

func TestParseEventUnknownType(t *testing.T) {
	line := `{"type":"file-history-snapshot","snapshot":{}}`
	ev := parseEvent(line)
	if ev.Type != "file-history-snapshot" {
		t.Errorf("Type = %q, want file-history-snapshot", ev.Type)
	}
	// Unknown types don't populate Text / StopReason / ProgressType.
	if ev.Text != "" || ev.StopReason != "" || ev.ProgressType != "" {
		t.Errorf("unexpected fields populated: %+v", ev)
	}
}

func TestParseEventAssistantNoMessage(t *testing.T) {
	// Malformed assistant event missing the message field. Should parse
	// as assistant with empty Text/StopReason, not crash.
	line := `{"type":"assistant"}`
	ev := parseEvent(line)
	if ev.Type != "assistant" {
		t.Errorf("Type = %q, want assistant", ev.Type)
	}
	if ev.Text != "" {
		t.Errorf("Text = %q, want empty", ev.Text)
	}
	if ev.StopReason != "" {
		t.Errorf("StopReason = %q, want empty", ev.StopReason)
	}
}

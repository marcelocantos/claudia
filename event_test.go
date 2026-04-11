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

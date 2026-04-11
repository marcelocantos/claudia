// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"strings"
)

// Event is a parsed JSONL event from a Claude Code session transcript.
type Event struct {
	// Type is the event type: "assistant", "user", "system", "progress", etc.
	Type string `json:"type"`

	// Raw is the complete JSONL line.
	Raw json.RawMessage `json:"-"`

	// Text is populated for type == "assistant" with the concatenated text
	// content blocks from the message.
	Text string `json:"-"`

	// StopReason is populated for type == "assistant" with the message's
	// stop_reason field. A single logical assistant message is split across
	// multiple JSONL events (one per content block); only the last event in
	// the message carries a stop_reason, earlier ones have it unset.
	// Terminal values are "end_turn", "stop_sequence", and "max_tokens";
	// "tool_use" means the model paused for tool results and will continue.
	StopReason string `json:"-"`

	// ProgressType is populated for type == "progress" (e.g. "tool_use").
	ProgressType string `json:"-"`
}

// IsTerminalStop reports whether the event represents a completed
// assistant turn — i.e. an assistant event whose stop_reason is one of
// the terminal values (end_turn, stop_sequence, max_tokens). A tool_use
// stop reason is intentionally excluded: the model paused for tool
// results and further assistant events will follow.
func (e Event) IsTerminalStop() bool {
	if e.Type != "assistant" {
		return false
	}
	switch e.StopReason {
	case "end_turn", "stop_sequence", "max_tokens":
		return true
	}
	return false
}

// EventFunc receives events from the session transcript.
type EventFunc func(Event)

func parseEvent(line string) Event {
	var ev Event
	ev.Raw = json.RawMessage(line)

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		ev.Type = "unknown"
		return ev
	}
	ev.Type, _ = entry["type"].(string)

	switch ev.Type {
	case "assistant":
		if msg, ok := entry["message"].(map[string]any); ok {
			if sr, ok := msg["stop_reason"].(string); ok {
				ev.StopReason = sr
			}
			if content, ok := msg["content"].([]any); ok {
				var texts []string
				for _, c := range content {
					if cm, ok := c.(map[string]any); ok && cm["type"] == "text" {
						if t, ok := cm["text"].(string); ok {
							texts = append(texts, t)
						}
					}
				}
				ev.Text = strings.Join(texts, "\n")
			}
		}
	case "progress":
		if data, ok := entry["data"].(map[string]any); ok {
			ev.ProgressType, _ = data["type"].(string)
		}
	}

	return ev
}

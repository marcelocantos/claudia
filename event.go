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
	// content blocks.
	Text string `json:"-"`

	// ProgressType is populated for type == "progress" (e.g. "tool_use").
	ProgressType string `json:"-"`
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

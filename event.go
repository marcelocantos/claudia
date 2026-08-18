// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"strings"
)

// Event is a provider-neutral event from a persistent agent session.
type Event struct {
	// Type is the event type: "assistant", "user", "system", "progress", etc.
	Type string `json:"type"`

	// SessionID is the backend's durable conversation identity: a Claude
	// session id, Codex thread id, or Grok ACP session id.
	SessionID string `json:"-"`

	// TurnID groups every event caused by one user prompt. It comes from the
	// backend's own correlation primitive: the Claude prompt record UUID,
	// Codex turn id, or Grok ACP session/prompt request id. Empty means the
	// backend did not associate the event with an in-flight turn.
	TurnID string `json:"-"`

	// MessageID is the backend's identity for a logical message or item when
	// one exists (Claude message.id, Codex itemId). It is empty for backends
	// and event kinds that expose only turn-level correlation.
	MessageID string `json:"-"`

	// RecordID is the identity of this individual backend record when one
	// exists. Claude transcript records expose their top-level uuid here;
	// other providers currently leave it empty.
	RecordID string `json:"-"`

	// Raw is the complete JSONL line as the bytes that were parsed.
	// Callers that want to re-marshal can wrap in json.RawMessage.
	Raw []byte `json:"-"`

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

	// Usage is populated for type == "assistant" with the token usage
	// reported by the message. Zero if the event carries no usage data.
	Usage Usage `json:"-"`

	// ProgressType is populated for type == "progress" (e.g. "tool_use").
	ProgressType string `json:"-"`

	// Model is populated for type == "assistant" with the model that actually
	// produced the message (the transcript's message.model), e.g.
	// "claude-opus-5". It is the model Claude Code resolved and ran — an alias
	// like "opus" resolves to its full id here. An unusable model surfaces as
	// Model == "<synthetic>" with IsError set (Claude Code does not fail at
	// launch). Compare against the model you requested to catch silent
	// fallback or drift. Empty on non-assistant events that carry no model.
	Model string `json:"-"`

	// IsError is true when the backend reports a turn/API failure on this
	// event (Claude model_not_found / is_api_error_message, Codex failed
	// turn, etc.). [Agent.WaitForResponse] returns a descriptive error
	// instead of treating the message as a normal reply or hanging.
	IsError bool `json:"-"`
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
	ev.Raw = []byte(line)

	var entry map[string]any
	if err := json.Unmarshal(ev.Raw, &entry); err != nil {
		ev.Type = "unknown"
		return ev
	}
	ev.Type, _ = entry["type"].(string)
	ev.RecordID, _ = entry["uuid"].(string)
	ev.SessionID, _ = entry["sessionId"].(string)
	if ev.SessionID == "" {
		ev.SessionID, _ = entry["session_id"].(string)
	}

	switch ev.Type {
	case "assistant":
		if msg, ok := entry["message"].(map[string]any); ok {
			ev.MessageID, _ = msg["id"].(string)
			if sr, ok := msg["stop_reason"].(string); ok {
				ev.StopReason = sr
			}
			if m, ok := msg["model"].(string); ok {
				ev.Model = m
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
		// Live shape (Claude Code ≥2.1.x): invalid --model is echoed on
		// init, then the turn emits an assistant event with
		// model "<synthetic>", top-level error "model_not_found", and
		// is_api_error_message true — not a clean launch rejection.
		if isAPI, ok := entry["is_api_error_message"].(bool); ok && isAPI {
			ev.IsError = true
		}
		if errStr, ok := entry["error"].(string); ok && errStr != "" {
			ev.IsError = true
		}
		if u, ok := entry["usage"].(map[string]any); ok {
			ev.Usage.InputTokens = int(jsonFloat(u, "input_tokens"))
			ev.Usage.OutputTokens = int(jsonFloat(u, "output_tokens"))
			ev.Usage.CacheCreationInputTokens = int(jsonFloat(u, "cache_creation_input_tokens"))
			ev.Usage.CacheReadInputTokens = int(jsonFloat(u, "cache_read_input_tokens"))
		}
	case "progress":
		if data, ok := entry["data"].(map[string]any); ok {
			ev.ProgressType, _ = data["type"].(string)
		}
	}

	return ev
}

// claudeEventCorrelator stamps transcript events with the UUID of the user
// prompt record that opened their turn. Claude repeats message.id across the
// content-block records of a terminal assistant message, so terminalMessageID
// keeps those sibling records in the same turn while ensuring the first
// unrelated record after them is not mislabeled with the completed turn.
type claudeEventCorrelator struct {
	turnID            string
	terminalMessageID string
}

func (c *claudeEventCorrelator) correlate(ev Event) Event {
	if isUserPromptLine(ev.Raw) {
		c.turnID = ev.RecordID
		c.terminalMessageID = ""
	} else if c.terminalMessageID != "" && ev.MessageID != c.terminalMessageID {
		c.turnID = ""
		c.terminalMessageID = ""
	}

	ev.TurnID = c.turnID
	if ev.IsTerminalStop() {
		if ev.MessageID == "" {
			c.turnID = ""
			c.terminalMessageID = ""
		} else {
			c.terminalMessageID = ev.MessageID
		}
	}
	return ev
}

func jsonFloat(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

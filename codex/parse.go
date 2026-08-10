// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"encoding/json"
	"fmt"
)

// parser maps `codex exec --json` JSONL lines to [Event] values.
// Stateful: keeps the last agent_message for turn.completed result text.
type parser struct {
	lastAgentMessage string
}

// ParseLine parses one NDJSON line. Unrecognised or malformed lines yield nil.
func (p *parser) ParseLine(line []byte) []Event {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		return nil
	}

	switch base.Type {
	case "thread.started":
		var msg struct {
			ThreadID string `json:"thread_id"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		return []Event{{Type: EventInit, SessionID: msg.ThreadID}}
	case "item.started", "item.completed":
		return p.parseItem(line)
	case "turn.completed":
		var msg struct {
			Usage struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		return []Event{{
			Type:    EventResult,
			Content: p.lastAgentMessage,
			Usage: Usage{
				InputTokens:          msg.Usage.InputTokens,
				OutputTokens:         msg.Usage.OutputTokens,
				CacheReadInputTokens: msg.Usage.CachedInputTokens,
			},
		}}
	case "turn.failed":
		var msg struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		return []Event{{Type: EventError, Error: ClassifyFailure(msg.Error)}}
	case "error":
		var msg struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		errMsg := msg.Message
		if errMsg == "" {
			errMsg = msg.Error
		}
		return []Event{{Type: EventError, Error: ClassifyFailure(errMsg)}}
	default:
		return nil
	}
}

func (p *parser) parseItem(line []byte) []Event {
	var msg struct {
		Type string `json:"type"`
		Item struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Status  string `json:"status"`
		} `json:"item"`
	}
	var raw struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}
	_ = json.Unmarshal(line, &raw)

	if msg.Type == "item.completed" && msg.Item.Type == "agent_message" && msg.Item.Text != "" {
		p.lastAgentMessage = msg.Item.Text
		return []Event{{Type: EventText, Content: msg.Item.Text}}
	}
	if msg.Item.Type == "command_execution" {
		input := string(raw.Item)
		if input == "" {
			input = fmt.Sprintf(`{"command":%q,"status":%q}`, msg.Item.Command, msg.Item.Status)
		}
		return []Event{{
			Type:      EventToolUse,
			ToolID:    msg.Item.ID,
			ToolName:  msg.Item.Type,
			ToolInput: input,
		}}
	}
	return nil
}

// ParseLines is a convenience for hermetic fixture tests: parse every line
// and return the flat event list.
func ParseLines(lines [][]byte) []Event {
	var p parser
	var out []Event
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		out = append(out, p.ParseLine(line)...)
	}
	return out
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"strings"
)

// ParseCursorTaskLine parses one NDJSON line from
// `agent --print --output-format stream-json`. The envelope matches Claude's
// stream-json closely, but usage fields are camelCase (inputTokens, …).
func ParseCursorTaskLine(line []byte) []TaskEvent {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		return nil
	}
	switch base.Type {
	case "system":
		return parseTaskSystem(line)
	case "assistant":
		return parseTaskAssistant(line)
	case "result":
		return parseCursorTaskResult(line)
	default:
		// thinking / user / tool progress: ignore for Task event surface
		return nil
	}
}

func parseCursorTaskResult(line []byte) []TaskEvent {
	var msg struct {
		Subtype    string  `json:"subtype"`
		Result     string  `json:"result"`
		IsError    bool    `json:"is_error"`
		DurationMs float64 `json:"duration_ms"`
		Usage      *struct {
			InputTokens      int `json:"inputTokens"`
			OutputTokens     int `json:"outputTokens"`
			CacheReadTokens  int `json:"cacheReadTokens"`
			CacheWriteTokens int `json:"cacheWriteTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}
	if msg.IsError || msg.Subtype != "success" {
		errMsg := strings.TrimSpace(msg.Result)
		if errMsg == "" {
			errMsg = "cursor task failed"
		}
		return []TaskEvent{{
			Type:     TaskEventError,
			IsError:  true,
			ErrorMsg: errMsg,
		}}
	}
	var usage Usage
	if msg.Usage != nil {
		usage = Usage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadTokens,
			CacheCreationInputTokens: msg.Usage.CacheWriteTokens,
		}
	}
	return []TaskEvent{{
		Type:       TaskEventResult,
		Content:    msg.Result,
		DurationMs: msg.DurationMs,
		Usage:      usage,
	}}
}

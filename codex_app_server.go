// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
)

type codexAppServerEvent struct {
	Raw        []byte
	Method     string
	ThreadID   string
	TurnID     string
	ItemID     string
	ItemType   string
	Command    string
	Text       string
	Status     string
	ErrorMsg   string
	Usage      Usage
	IsResponse bool
	IsError    bool
}

func parseCodexAppServerLine(line []byte) (codexAppServerEvent, bool, error) {
	var msg struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Thread *struct {
				ID string `json:"id"`
			} `json:"thread"`
			Turn *struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		} `json:"result"`
		Params *struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Thread   *struct {
				ID string `json:"id"`
			} `json:"thread"`
			Turn *struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
			Item *struct {
				ID      string `json:"id"`
				Type    string `json:"type"`
				Command string `json:"command"`
				Text    string `json:"text"`
				Status  string `json:"status"`
			} `json:"item"`
			Usage *struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return codexAppServerEvent{}, false, fmt.Errorf("parse codex app-server line: %w", err)
	}
	if msg.Error != nil {
		return codexAppServerEvent{
			Raw:        append([]byte(nil), line...),
			ErrorMsg:   msg.Error.Message,
			IsResponse: msg.ID != nil,
			IsError:    true,
		}, true, nil
	}
	if msg.Result != nil {
		ev := codexAppServerEvent{
			Raw:        append([]byte(nil), line...),
			IsResponse: msg.ID != nil,
		}
		if msg.Result.Thread != nil {
			ev.Method = "thread/start"
			ev.ThreadID = msg.Result.Thread.ID
			return ev, true, nil
		}
		if msg.Result.Turn != nil {
			ev.Method = "turn/start"
			ev.TurnID = msg.Result.Turn.ID
			ev.Status = msg.Result.Turn.Status
			return ev, true, nil
		}
		return ev, true, nil
	}
	if msg.Method == "" || msg.Params == nil {
		return codexAppServerEvent{}, false, nil
	}
	ev := codexAppServerEvent{
		Raw:      append([]byte(nil), line...),
		Method:   msg.Method,
		ThreadID: msg.Params.ThreadID,
		TurnID:   msg.Params.TurnID,
	}
	if msg.Params.Thread != nil {
		ev.ThreadID = msg.Params.Thread.ID
	}
	if msg.Params.Turn != nil {
		ev.TurnID = msg.Params.Turn.ID
		ev.Status = msg.Params.Turn.Status
	}
	if msg.Params.Item != nil {
		ev.ItemID = msg.Params.Item.ID
		ev.ItemType = msg.Params.Item.Type
		ev.Command = msg.Params.Item.Command
		ev.Text = msg.Params.Item.Text
		ev.Status = msg.Params.Item.Status
	}
	if msg.Params.Usage != nil {
		ev.Usage = Usage{
			InputTokens:          msg.Params.Usage.InputTokens,
			CacheReadInputTokens: msg.Params.Usage.CachedInputTokens,
			OutputTokens:         msg.Params.Usage.OutputTokens,
		}
	}
	if msg.Params.Error != nil {
		ev.ErrorMsg = msg.Params.Error.Message
		ev.IsError = true
	}
	return ev, true, nil
}

func (ev codexAppServerEvent) agentEvent() (Event, bool) {
	if ev.IsError {
		return Event{
			Type: "system",
			Raw:  ev.Raw,
			Text: ev.ErrorMsg,
		}, true
	}
	switch ev.Method {
	case "item/started", "item/completed":
		if ev.ItemType == "command_execution" {
			return Event{
				Type:         "progress",
				Raw:          ev.Raw,
				ProgressType: "tool_use",
			}, true
		}
		if ev.ItemType == "agent_message" {
			return Event{
				Type: "assistant",
				Raw:  ev.Raw,
				Text: ev.Text,
			}, true
		}
	case "turn/completed":
		out := Event{
			Type:  "assistant",
			Raw:   ev.Raw,
			Usage: ev.Usage,
		}
		switch ev.Status {
		case "completed", "interrupted":
			out.StopReason = "end_turn"
		case "failed":
			return Event{
				Type: "system",
				Raw:  ev.Raw,
				Text: ev.ErrorMsg,
			}, true
		}
		return out, true
	}
	return Event{}, false
}

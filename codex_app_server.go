// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
)

// codexAppServerEvent is the hermetic parse of one app-server JSONL line
// (JSON-RPC response or notification). Field names match the public contract
// proven by the 🎯T4.4 live spike against codex-cli 0.146.0-alpha.9.2.
type codexAppServerEvent struct {
	Raw            []byte
	Method         string
	ThreadID       string
	TurnID         string
	ItemID         string
	ItemType       string
	Command        string
	Text           string
	Delta          string
	Status         string
	ErrorMsg       string
	Model          string
	ApprovalPolicy string
	SandboxType    string
	CWD            string
	Usage          Usage
	IsResponse     bool
	IsError        bool
}

// codexAppServerRequest is a JSON-RPC request/notification envelope for the
// public app-server methods. Built by the helpers below so Session mode does
// not invent field names.
type codexAppServerRequest struct {
	Method string `json:"method"`
	ID     *int   `json:"id,omitempty"`
	Params any    `json:"params,omitempty"`
}

// codexAppServerClientInfo is the initialize clientInfo payload.
type codexAppServerClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// codexAppServerThreadStartParams are thread/start params (schema 0.146+).
// Prefer sandbox (not sandboxPolicy) on thread/start; permissions cannot be
// combined with sandbox.
type codexAppServerThreadStartParams struct {
	CWD            string `json:"cwd,omitempty"`
	Model          string `json:"model,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	Ephemeral      *bool  `json:"ephemeral,omitempty"`
}

// codexAppServerTurnStartParams are turn/start params.
type codexAppServerTurnStartParams struct {
	ThreadID       string                    `json:"threadId"`
	Input          []codexAppServerUserInput `json:"input"`
	ApprovalPolicy string                    `json:"approvalPolicy,omitempty"`
	Model          string                    `json:"model,omitempty"`
	CWD            string                    `json:"cwd,omitempty"`
	SandboxPolicy  any                       `json:"sandboxPolicy,omitempty"`
}

// codexAppServerUserInput is one turn/start input element.
type codexAppServerUserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// codexAppServerThreadIDParams is the threadId-only envelope shared by
// thread/resume, thread/fork, thread/archive, and thread/unarchive.
type codexAppServerThreadIDParams struct {
	ThreadID       string `json:"threadId"`
	CWD            string `json:"cwd,omitempty"`
	Model          string `json:"model,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
}

// codexAppServerTurnInterruptParams is turn/interrupt.
type codexAppServerTurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

func codexAppServerInitialize(id int, info codexAppServerClientInfo) codexAppServerRequest {
	return codexAppServerRequest{
		Method: "initialize",
		ID:     intPtr(id),
		Params: map[string]any{"clientInfo": info},
	}
}

func codexAppServerInitialized() codexAppServerRequest {
	return codexAppServerRequest{
		Method: "initialized",
		Params: map[string]any{},
	}
}

func codexAppServerThreadStart(id int, params codexAppServerThreadStartParams) codexAppServerRequest {
	return codexAppServerRequest{Method: "thread/start", ID: intPtr(id), Params: params}
}

func codexAppServerTurnStart(id int, params codexAppServerTurnStartParams) codexAppServerRequest {
	return codexAppServerRequest{Method: "turn/start", ID: intPtr(id), Params: params}
}

func codexAppServerTurnInterrupt(id int, threadID, turnID string) codexAppServerRequest {
	return codexAppServerRequest{
		Method: "turn/interrupt",
		ID:     intPtr(id),
		Params: codexAppServerTurnInterruptParams{ThreadID: threadID, TurnID: turnID},
	}
}

func codexAppServerThreadResume(id int, params codexAppServerThreadIDParams) codexAppServerRequest {
	return codexAppServerRequest{Method: "thread/resume", ID: intPtr(id), Params: params}
}

func codexAppServerThreadFork(id int, params codexAppServerThreadIDParams) codexAppServerRequest {
	return codexAppServerRequest{Method: "thread/fork", ID: intPtr(id), Params: params}
}

func codexAppServerThreadArchive(id int, threadID string) codexAppServerRequest {
	return codexAppServerRequest{
		Method: "thread/archive",
		ID:     intPtr(id),
		Params: codexAppServerThreadIDParams{ThreadID: threadID},
	}
}

func codexAppServerThreadUnarchive(id int, threadID string) codexAppServerRequest {
	return codexAppServerRequest{
		Method: "thread/unarchive",
		ID:     intPtr(id),
		Params: codexAppServerThreadIDParams{ThreadID: threadID},
	}
}

func intPtr(v int) *int { return &v }

func parseCodexAppServerLine(line []byte) (codexAppServerEvent, bool, error) {
	var msg struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Thread *struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionId"`
			} `json:"thread"`
			// Model is the resolved model id on thread/start (sibling of thread).
			Model          string `json:"model"`
			ApprovalPolicy string `json:"approvalPolicy"`
			CWD            string `json:"cwd"`
			Sandbox        *struct {
				Type string `json:"type"`
			} `json:"sandbox"`
			Turn *struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		} `json:"result"`
		Params *struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
			Thread   *struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionId"`
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
			// Hermetic fixtures put usage on turn/completed (snake_case tokens).
			Usage *struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage"`
			// Live 0.146+ emits thread/tokenUsage/updated with camelCase totals.
			TokenUsage *struct {
				Total *struct {
					InputTokens           int `json:"inputTokens"`
					CachedInputTokens     int `json:"cachedInputTokens"`
					OutputTokens          int `json:"outputTokens"`
					ReasoningOutputTokens int `json:"reasoningOutputTokens"`
				} `json:"total"`
			} `json:"tokenUsage"`
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
			Raw:            append([]byte(nil), line...),
			IsResponse:     msg.ID != nil,
			Model:          msg.Result.Model,
			ApprovalPolicy: msg.Result.ApprovalPolicy,
			CWD:            msg.Result.CWD,
		}
		if msg.Result.Sandbox != nil {
			ev.SandboxType = msg.Result.Sandbox.Type
		}
		if msg.Result.Thread != nil {
			ev.Method = "thread/start"
			ev.ThreadID = msg.Result.Thread.ID
			if ev.ThreadID == "" {
				ev.ThreadID = msg.Result.Thread.SessionID
			}
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
		ItemID:   msg.Params.ItemID,
		Delta:    msg.Params.Delta,
	}
	if msg.Params.Thread != nil {
		ev.ThreadID = msg.Params.Thread.ID
		if ev.ThreadID == "" {
			ev.ThreadID = msg.Params.Thread.SessionID
		}
	}
	if msg.Params.Turn != nil {
		ev.TurnID = msg.Params.Turn.ID
		ev.Status = msg.Params.Turn.Status
	}
	if msg.Params.Item != nil {
		ev.ItemID = msg.Params.Item.ID
		ev.ItemType = normalizeCodexAppServerItemType(msg.Params.Item.Type)
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
	if msg.Params.TokenUsage != nil && msg.Params.TokenUsage.Total != nil {
		t := msg.Params.TokenUsage.Total
		ev.Usage = Usage{
			InputTokens:          t.InputTokens,
			CacheReadInputTokens: t.CachedInputTokens,
			OutputTokens:         t.OutputTokens,
		}
	}
	if msg.Params.Error != nil {
		ev.ErrorMsg = msg.Params.Error.Message
		ev.IsError = true
	}
	return ev, true, nil
}

// normalizeCodexAppServerItemType maps live camelCase item types (0.146+) onto
// the stable snake_case names used by hermetic fixtures and agentEvent.
func normalizeCodexAppServerItemType(t string) string {
	switch t {
	case "agentMessage":
		return "agent_message"
	case "commandExecution":
		return "command_execution"
	case "userMessage":
		return "user_message"
	default:
		return t
	}
}

func (ev codexAppServerEvent) agentEvent() (Event, bool) {
	if ev.IsError {
		return Event{
			Type:    "system",
			Raw:     ev.Raw,
			Text:    ev.ErrorMsg,
			IsError: true,
		}, true
	}
	switch ev.Method {
	case "thread/start":
		// Publish resolved model from thread/start so Agent.Model() is set
		// before the first assistant turn (Codex does not put model on items).
		if ev.Model == "" && ev.ThreadID == "" {
			return Event{}, false
		}
		return Event{
			Type:  "system",
			Raw:   ev.Raw,
			Model: ev.Model,
			Text:  ev.ThreadID,
		}, true
	case "item/started", "item/completed":
		if ev.ItemType == "command_execution" {
			return Event{
				Type:         "progress",
				Raw:          ev.Raw,
				ProgressType: "tool_use",
			}, true
		}
		if ev.ItemType == "agent_message" {
			// item/started often has empty text; only emit assistant on completed
			// or when text is present so WaitForResponse does not race empty deltas.
			if ev.Method == "item/started" && ev.Text == "" {
				return Event{}, false
			}
			return Event{
				Type:  "assistant",
				Raw:   ev.Raw,
				Text:  ev.Text,
				Model: ev.Model,
			}, true
		}
	case "thread/tokenUsage/updated":
		if ev.Usage.InputTokens == 0 && ev.Usage.OutputTokens == 0 && ev.Usage.CacheReadInputTokens == 0 {
			return Event{}, false
		}
		return Event{
			Type:  "assistant",
			Raw:   ev.Raw,
			Usage: ev.Usage,
		}, true
	case "turn/completed":
		out := Event{
			Type:  "assistant",
			Raw:   ev.Raw,
			Usage: ev.Usage,
			Model: ev.Model,
		}
		switch ev.Status {
		case "completed", "interrupted":
			out.StopReason = "end_turn"
		case "failed":
			return Event{
				Type:    "system",
				Raw:     ev.Raw,
				Text:    ev.ErrorMsg,
				IsError: true,
			}, true
		}
		return out, true
	}
	return Event{}, false
}

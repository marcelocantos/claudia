// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ACP ProgressType values published on the existing Event stream (🎯T50).
const (
	ProgressThought        = "thought"
	ProgressPlan           = "plan"
	ProgressPromptAccepted = "prompt_accepted"
	ProgressPermission     = "permission"
	ProgressToolUse        = "tool_use"
	// ProgressPromptSuperseded: a steered-over session/prompt returned
	// its JSON-RPC result while the turn continued on the steer's id
	// (🎯T72.1). Raw is that result; it is never a terminal stop.
	ProgressPromptSuperseded = "prompt_superseded"
)

type acpUpdateProbe struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Title      string `json:"title"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		ToolCallID string `json:"toolCallId"`
		Entries    []struct {
			Content string `json:"content"`
		} `json:"entries"`
		Meta *struct {
			TotalTokens int `json:"totalTokens"`
		} `json:"_meta"`
	} `json:"update"`
	Meta *struct {
		TotalTokens int `json:"totalTokens"`
	} `json:"_meta"`
}

func parseACPUpdate(params []byte) (acpUpdateProbe, bool) {
	var p acpUpdateProbe
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return p, false
	}
	return p, true
}

func acpContentText(p acpUpdateProbe) string {
	if p.Update.Content != nil {
		return p.Update.Content.Text
	}
	return ""
}

func acpPlanText(p acpUpdateProbe) string {
	if t := acpContentText(p); t != "" {
		return t
	}
	var parts []string
	for _, e := range p.Update.Entries {
		if s := strings.TrimSpace(e.Content); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

func acpUsage(p acpUpdateProbe) Usage {
	n := 0
	if p.Update.Meta != nil {
		n = p.Update.Meta.TotalTokens
	}
	if n == 0 && p.Meta != nil {
		n = p.Meta.TotalTokens
	}
	if n <= 0 {
		return Usage{}
	}
	return Usage{OutputTokens: n}
}

func acpToolIdentity(p acpUpdateProbe) (id, title, status string) {
	id = strings.TrimSpace(p.Update.ToolCallID)
	title = strings.TrimSpace(p.Update.Title)
	if title == "" {
		title = strings.TrimSpace(p.Update.Name)
	}
	status = strings.TrimSpace(p.Update.Status)
	return id, title, status
}

func acpProgressEvent(sessionID, turnID string, params []byte) (Event, bool) {
	p, ok := parseACPUpdate(params)
	if !ok {
		return Event{}, false
	}
	base := Event{
		SessionID: sessionID,
		TurnID:    turnID,
		Raw:       params,
		Usage:     acpUsage(p),
	}
	switch p.Update.SessionUpdate {
	case "agent_thought_chunk":
		base.Type = "progress"
		base.ProgressType = ProgressThought
		base.Text = acpContentText(p)
		return base, true
	case "plan":
		base.Type = "progress"
		base.ProgressType = ProgressPlan
		base.Text = acpPlanText(p)
		return base, true
	case "tool_call", "tool_call_update":
		id, title, status := acpToolIdentity(p)
		base.Type = "progress"
		base.ProgressType = ProgressToolUse
		base.ToolCallID = id
		base.ToolTitle = title
		base.ToolStatus = status
		return base, true
	}
	return Event{}, false
}

func acpPromptAcceptedEvent(sessionID string, promptID int64) Event {
	return Event{
		Type:         "progress",
		SessionID:    sessionID,
		TurnID:       strconv.FormatInt(promptID, 10),
		ProgressType: ProgressPromptAccepted,
	}
}

func acpPromptSupersededEvent(sessionID string, promptID int64, msg acpRPCMessage) Event {
	raw := msg.Result
	if msg.Error != nil {
		raw, _ = json.Marshal(msg.Error)
	}
	return Event{
		Type:         "progress",
		SessionID:    sessionID,
		TurnID:       strconv.FormatInt(promptID, 10),
		Raw:          raw,
		ProgressType: ProgressPromptSuperseded,
	}
}

func acpPermissionEvent(sessionID string, promptID int64, params []byte) Event {
	turn := ""
	if promptID != 0 {
		turn = strconv.FormatInt(promptID, 10)
	}
	return Event{
		Type:         "progress",
		SessionID:    sessionID,
		TurnID:       turn,
		Raw:          params,
		ProgressType: ProgressPermission,
	}
}

func publishEvent(onEvent EventFunc, ev Event) {
	if onEvent == nil {
		return
	}
	onEvent(ev)
}

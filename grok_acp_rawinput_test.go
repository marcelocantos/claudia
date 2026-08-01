// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"strings"
	"testing"
)

// 🎯T64 (jevons): tool_call session/update must keep rawInput on Event.Raw
// after handleSessionUpdate — not drop it via re-marshal of a narrow struct.
func TestGrokACPToolCallPreservesRawInput(t *testing.T) {
	params := json.RawMessage(`{
		"sessionId":"s1",
		"update":{
			"sessionUpdate":"tool_call",
			"title":"search_tool",
			"kind":"other",
			"rawInput":{"query":"jevonsmcp agent list","limit":3}
		}
	}`)

	var got []Event
	c := &grokACPClient{
		onEvent: func(ev Event) { got = append(got, ev) },
	}
	c.handleSessionUpdate(params)
	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}
	ev := got[0]
	if ev.Type != "progress" || ev.ProgressType != "tool_use" {
		t.Fatalf("type=%q progress=%q", ev.Type, ev.ProgressType)
	}
	if !strings.Contains(string(ev.Raw), "rawInput") {
		t.Fatalf("Event.Raw lost rawInput: %s", ev.Raw)
	}
	if !strings.Contains(string(ev.Raw), "jevonsmcp agent list") {
		t.Fatalf("Event.Raw lost query value: %s", ev.Raw)
	}
	// Structural: unmarshalled update.rawInput still present
	var probe struct {
		Update struct {
			Title    string         `json:"title"`
			RawInput map[string]any `json:"rawInput"`
		} `json:"update"`
	}
	if err := json.Unmarshal(ev.Raw, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Update.Title != "search_tool" {
		t.Fatalf("title=%q", probe.Update.Title)
	}
	if probe.Update.RawInput["query"] != "jevonsmcp agent list" {
		t.Fatalf("rawInput=%v", probe.Update.RawInput)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

func TestNormalizeSidecarToolEndStatus(t *testing.T) {
	cases := map[string]string{
		"completed":  "completed",
		"failed":     "failed",
		"cancelled":  "cancelled",
		"denied":     "denied",
		"unknown":    "unknown",
		"":           "unknown",
		"garbage":    "unknown",
		" completed ": "completed",
	}
	for in, want := range cases {
		if got := NormalizeSidecarToolEndStatus(in); got != want {
			t.Fatalf("NormalizeSidecarToolEndStatus(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestPublishSidecarToolProgressContract(t *testing.T) {
	start := PublishSidecarToolStart("call-1", "Bash", `{"command":"true"}`)
	if start.Raw != nil || start.ToolCallID != "call-1" || start.ToolStatus != "in_progress" {
		t.Fatalf("start = %+v; want Raw=nil in_progress call-1", start)
	}
	end := PublishSidecarToolEnd("call-1", "Bash", "ok", "cancelled")
	if end.Raw != nil || end.ToolCallID != "call-1" || end.ToolStatus != "cancelled" {
		t.Fatalf("end = %+v; want Raw=nil cancelled call-1", end)
	}
	wire, err := EncodeEventWire(end)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["raw"] != nil {
		t.Fatalf("wire raw = %v; want absent", decoded["raw"])
	}
	if decoded["tool_call_id"] != "call-1" || decoded["tool_status"] != "cancelled" {
		t.Fatalf("wire = %v; want same id + cancelled status", decoded)
	}
}

func TestSidecarToolWirePump(t *testing.T) {
	lines := []struct {
		line string
		want string
	}{
		{`{"type":"tool_start","call_id":"c1","name":"Read","text":"{}"}`, "in_progress"},
		{`{"type":"tool_end","call_id":"c1","name":"Read","text":"ok","status":"completed"}`, "completed"},
		{`{"type":"tool_end","call_id":"c1","name":"Read","text":"nope","status":"failed"}`, "failed"},
		{`{"type":"tool_end","call_id":"c1","name":"Read","text":"skip","status":"cancelled"}`, "cancelled"},
		{`{"type":"tool_end","call_id":"c1","name":"Read","text":"blocked","status":"denied"}`, "denied"},
		{`{"type":"tool_end","call_id":"c1","name":"Read","text":"?","status":"mystery"}`, "unknown"},
	}
	for _, tc := range lines {
		var ev omp.Event
		if err := json.Unmarshal([]byte(tc.line), &ev); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.line, err)
		}
		var host Event
		switch ev.Type {
		case "tool_start":
			host = PublishSidecarToolStart(ev.CallID, ev.Name, ev.Text)
		case "tool_end":
			host = PublishSidecarToolEnd(ev.CallID, ev.Name, ev.Text, ev.Status)
		default:
			t.Fatalf("unexpected type %q", ev.Type)
		}
		if host.Raw != nil {
			t.Fatalf("host Raw set for %q", tc.line)
		}
		if host.ToolStatus != tc.want {
			t.Fatalf("line %q: ToolStatus=%q want %q", tc.line, host.ToolStatus, tc.want)
		}
		if host.ToolCallID != "c1" {
			t.Fatalf("line %q: ToolCallID=%q", tc.line, host.ToolCallID)
		}
	}
}

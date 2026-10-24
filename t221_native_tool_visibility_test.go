// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// 🎯T221: native Cursor exec tool_start/tool_end from the sidecar reach the
// host as tool_use progress with ToolStatus (jevons 🎯T1218).
func TestT221NativeToolStartEndPublishesToolStatus(t *testing.T) {
	startFakeSidecar(t, []string{
		`{"type":"accepted"}`,
		`{"type":"tool_start","call_id":"call-native-1","name":"bash","text":"{\"command\":\"sleep 1\"}"}`,
		`{"type":"tool_end","call_id":"call-native-1","name":"bash","text":"ok","status":"completed"}`,
		`{"type":"text","text":"done"}`,
		`{"type":"turn_end","snapshot":{"messages":[]}}`,
	})
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: SubscriptionSeatProvider(ProviderClaude), Name: "t221-seat",
		Model: "m", WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()

	var mu sync.Mutex
	var tools []Event
	agent.SubscribeEvents(func(ev Event) {
		if ev.Type == "progress" && ev.ProgressType == "tool_use" {
			mu.Lock()
			tools = append(tools, ev)
			mu.Unlock()
		}
	})
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := agent.Send("go"); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.WaitForResponse(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tools) != 2 {
		t.Fatalf("tool_use progress events = %+v; want start + end", tools)
	}
	if tools[0].ToolCallID != "call-native-1" || tools[0].ToolTitle != "bash" || tools[0].ToolStatus != "in_progress" {
		t.Fatalf("start event = %+v; want in_progress bash call-native-1", tools[0])
	}
	if tools[1].ToolCallID != "call-native-1" || tools[1].ToolTitle != "bash" || tools[1].ToolStatus != "completed" {
		t.Fatalf("end event = %+v; want completed bash call-native-1", tools[1])
	}
}

func TestT221NativeToolEndStatuses(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		{"completed", "completed"},
		{"failed", "failed"},
		{"cancelled", "cancelled"},
		{"denied", "denied"},
		{"", "unknown"},
		{"mystery", "unknown"},
	}
	for _, tc := range cases {
		startFakeSidecar(t, []string{
			`{"type":"accepted"}`,
			`{"type":"tool_start","call_id":"c","name":"bash","text":"{}"}`,
			fmt.Sprintf(`{"type":"tool_end","call_id":"c","name":"bash","text":"x","status":"%s"}`, tc.status),
			`{"type":"turn_end","snapshot":{"messages":[]}}`,
		})
		ctx := context.Background()
		agent, err := startDirectContext(ctx, Config{
			Provider: SubscriptionSeatProvider(ProviderClaude), Name: "t221-seat",
			Model: "m", WorkDir: t.TempDir(), TermLogPath: "-",
		})
		if err != nil {
			t.Fatal(err)
		}
		defer agent.Stop()
		var last Event
		agent.SubscribeEvents(func(ev Event) {
			if ev.Type == "progress" && ev.ProgressType == "tool_use" && ev.ToolStatus != "in_progress" {
				last = ev
			}
		})
		if err := agent.WaitReady(ctx); err != nil {
			t.Fatal(err)
		}
		if err := agent.Send("go"); err != nil {
			t.Fatal(err)
		}
		if _, err := agent.WaitForResponse(ctx); err != nil {
			t.Fatal(err)
		}
		if last.ToolCallID != "c" || last.ToolStatus != tc.want || last.Raw != nil {
			t.Fatalf("status %q: last=%+v want ToolStatus=%q Raw=nil", tc.status, last, tc.want)
		}
	}
}

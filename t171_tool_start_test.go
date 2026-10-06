// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 🎯T171: a sidecar seat's own tool call (Bash, Read, Glob, Grep) reaches
// the host as a tool_use progress event when it starts, so a long Bash call
// is visible while it runs instead of the seat going silent.
func TestT171SidecarOwnToolStartPublishesToolUse(t *testing.T) {
	startFakeTransferSidecar(t, []string{
		`{"type":"accepted"}`,
		`{"type":"tool_start","call_id":"call-1","name":"Bash","text":"{\"command\":\"sleep 60\"}"}`,
		`{"type":"text","text":"ok"}`,
		`{"type":"turn_end","snapshot":{"messages":[]}}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agent, err := startDirectContext(ctx, Config{
		Provider: SubscriptionSeatProvider(ProviderClaude), Name: "t171-seat",
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
	if len(tools) != 1 {
		t.Fatalf("tool_use progress events = %+v; want one for the Bash start", tools)
	}
	got := tools[0]
	if got.ToolCallID != "call-1" || got.ToolTitle != "Bash" || got.Text != `{"command":"sleep 60"}` {
		t.Fatalf("tool_use event = %+v; want call-1 / Bash / the command args", got)
	}
}

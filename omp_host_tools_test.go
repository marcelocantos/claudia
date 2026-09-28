// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 🎯T886: a sidecar work seat is offered its host's jevons_* tools, with
// their own descriptions and schemas, from the seat's HTTP MCP server.
func TestHostJevonsToolsListsOnlyJevonsToolsWithSchemas(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
		`{"name":"jevons_agent_send","description":"Send to an agent","inputSchema":{"type":"object","properties":{"name":{"type":"string"}}}},` +
		`{"name":"playwright_click","description":"not ours","inputSchema":{"type":"object"}}]}}`
	for _, sse := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/not-ours" {
				t.Error("asked a server that is not the host's jevons server")
			}
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"tools/list"`) {
				t.Errorf("asked %s, want tools/list", b)
			}
			if sse {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message\ndata: "+list+"\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, list)
		}))
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		raw := hostJevonsTools(context.Background(), []MCPServer{
			{Name: "stdio", Command: "true"},
			{Name: "jevons-gone", Type: "http", URL: dead.URL},
			{Name: "playwright", Type: "http", URL: srv.URL + "/not-ours"},
			{Name: "jevonsmcp", Type: "http", URL: srv.URL},
		})
		srv.Close()
		var got []hostJevonsTool
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("sse=%v: %v (%s)", sse, err, raw)
		}
		if len(got) != 1 || got[0].Name != "jevons_agent_send" || got[0].Description != "Send to an agent" ||
			!strings.Contains(string(got[0].InputSchema), `"properties"`) {
			t.Fatalf("sse=%v: tools = %+v", sse, got)
		}
	}
	if raw := hostJevonsTools(context.Background(), nil); raw != nil {
		t.Fatalf("no servers should offer no tools, got %s", raw)
	}
}

// A prompt that reaches a busy seat is queued as a pi-agent-core follow-up,
// not refused: the refusal reached the owner as the seat's reply and the
// message was lost (2026-09-28). Leftover follow-ups drain inside the turn.
func TestOMPSidecarQueuesPromptWhileBusy(t *testing.T) {
	seat, err := os.ReadFile("sidecar/seat.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(seat)
	if strings.Contains(src, "seat is already processing") {
		t.Fatal("a busy seat must not refuse a prompt")
	}
	if !strings.Contains(src, "if ((turn && !turn.closed) || agent.state.isStreaming) {\n        agent.followUp({") {
		t.Fatal("a prompt that arrives mid-turn must be queued with agent.followUp")
	}
	if !strings.Contains(src, "agent.hasQueuedMessages() && !agent.state.isStreaming") || !strings.Contains(src, "await agent.continue()") {
		t.Fatal("follow-ups left after the run must drain inside the turn")
	}
}

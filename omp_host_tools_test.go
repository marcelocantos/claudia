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

// 🎯T871.1: every eligible server in AgentDef.MCPServers is advertised as a
// model-visible tool, not only a server named "jevons"; a plugin-shaped
// server (stdio, or an entry with only a Command, no http URL) is skipped.
func TestHostToolsAdvertisesEveryEligibleServer(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":1,"result":{"tools":[` +
		`{"name":"jevons_agent_send","description":"Send to an agent","inputSchema":{"type":"object","properties":{"name":{"type":"string"}}}},` +
		`{"name":"playwright_click","description":"not ours","inputSchema":{"type":"object"}}]}}`
	for _, sse := range []bool{false, true} {
		asked := map[string]bool{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			// The session handshake a listing opens with (🎯T147).
			if strings.Contains(string(b), `"initialize"`) {
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
				return
			}
			if strings.Contains(string(b), `"notifications/initialized"`) {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			asked[r.URL.Path] = true
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
		raw, routes := hostTools(context.Background(), []MCPServer{
			{Name: "plugin", Command: "true"},                           // plugin-shaped: no URL, not registered
			{Name: "jevons-gone", Type: "http", URL: dead.URL},          // eligible but silent: contributes nothing
			{Name: "playwright", Type: "http", URL: srv.URL + "/tools"}, // eligible: not filtered by server name any more
		})
		srv.Close()
		if !asked["/tools"] {
			t.Fatalf("sse=%v: the non-jevons server was never asked", sse)
		}
		var got []hostJevonsTool
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("sse=%v: %v (%s)", sse, err, raw)
		}
		if len(got) != 2 {
			t.Fatalf("sse=%v: tools = %+v, want both jevons_agent_send and playwright_click", sse, got)
		}
		names := map[string]hostJevonsTool{}
		for _, tool := range got {
			names[tool.Name] = tool
		}
		if tool, ok := names["jevons_agent_send"]; !ok || tool.Description != "Send to an agent" ||
			!strings.Contains(string(tool.InputSchema), `"properties"`) {
			t.Fatalf("sse=%v: jevons_agent_send = %+v", sse, tool)
		}
		if _, ok := names["playwright_click"]; !ok {
			t.Fatalf("sse=%v: playwright_click missing — non-jevons tools must be advertised too", sse)
		}
		if routes["jevons_agent_send"] != srv.URL+"/tools" || routes["playwright_click"] != srv.URL+"/tools" {
			t.Fatalf("sse=%v: routes = %+v, want both routed to %s", sse, routes, srv.URL+"/tools")
		}
		if _, ok := routes["true"]; ok {
			t.Fatalf("sse=%v: the plugin-shaped entry must not be registered: routes = %+v", sse, routes)
		}
	}
	if raw, routes := hostTools(context.Background(), nil); raw != nil || routes != nil {
		t.Fatalf("no servers should offer no tools, got %s / %+v", raw, routes)
	}
}

// 🎯T871.1: a hermetic seat whose MCPServers includes one HTTP fixture
// server can call a named tool on it and get the fixture result — the
// structured tools/call, not the jevons_*-only resolveFallbackTool runner.
func TestRunToolRoutesToDiscoveredServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		switch req.Method {
		case "initialize":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
		case "tools/list":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
				`{"name":"fixture_tool","description":"fixture","inputSchema":{"type":"object"}}]}}`)
		case "tools/call":
			if req.Params.Name != "fixture_tool" {
				t.Errorf("called %q, want fixture_tool", req.Params.Name)
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"fixture result"}]}}`)
		}
	}))
	defer srv.Close()

	_, routes := hostTools(context.Background(), []MCPServer{
		{Name: "fixture", Type: "http", URL: srv.URL},
	})
	ctrl := &ompControl{toolServers: routes}
	got := ctrl.runTool("fixture_tool", "call-1", "{}")
	if got != "fixture result" {
		t.Fatalf("runTool = %q, want the fixture's own result", got)
	}

	// A name the discovery never routed still falls back to the jevons-only
	// runner (🎯T864.3): it does not silently reach the fixture server,
	// and that runner does not invent an endpoint.
	prevExec := ompToolExec
	ompToolExec = nil
	t.Cleanup(func() { ompToolExec = prevExec })
	got = ctrl.runTool("jevons_never_advertised", "call-2", "{}")
	if strings.Contains(got, "fixture result") || !strings.Contains(got, "no tool endpoint configured") {
		t.Fatalf("unrouted name = %q, want a refusal that did not reach the fixture", got)
	}
}

// An unrouted tool has no implicit consumer endpoint. JEVONS_MCP_URL and
// the development MCP port are not consulted. A caller that passes a URL
// to CallMCPTool still reaches that server, for any tool name.
func TestUnroutedToolDoesNotDialImplicitEndpoint(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		switch req.Method {
		case "initialize":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if req.Params.Name != "bash" {
				t.Errorf("called %q", req.Params.Name)
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"explicit result"}]}}`)
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
	}))
	defer srv.Close()

	prev := ompToolExec
	t.Cleanup(func() { ompToolExec = prev })

	for _, env := range []string{srv.URL, ""} {
		t.Setenv("JEVONS_MCP_URL", env)
		hits = 0
		ompToolExec = nil
		if got := DefaultOMPToolExec("jevons_agent_list", "call-1", "{}"); got != `omp: no tool endpoint configured for "jevons_agent_list"` {
			t.Fatalf("JEVONS_MCP_URL=%q DefaultOMPToolExec = %q", env, got)
		}
		ctrl := &ompControl{}
		if got := ctrl.runTool("jevons_agent_list", "call-2", "{}"); got != `omp: no tool endpoint configured for "jevons_agent_list"` {
			t.Fatalf("JEVONS_MCP_URL=%q nil runner = %q", env, got)
		}
		ompToolExec = DefaultOMPToolExec
		if got := ctrl.runTool("jevons_agent_list", "call-3", "{}"); got != `omp: no tool endpoint configured for "jevons_agent_list"` {
			t.Fatalf("JEVONS_MCP_URL=%q installed runner = %q", env, got)
		}
		if hits != 0 {
			t.Fatalf("JEVONS_MCP_URL=%q dialed the implicit server %d times", env, hits)
		}
	}

	hits = 0
	if got := CallMCPTool(srv.URL, "bash", "{}"); got != "explicit result" {
		t.Fatalf("CallMCPTool = %q, want the explicit server's result", got)
	}
	if hits == 0 {
		t.Fatal("explicit URL was not contacted")
	}
}

// 🎯T871.1: the sidecar must offer the model every host tool it is given,
// not only jevons_* names — routing which server executes a tool is the
// host's job (hostTools), not seat.ts's.
func TestSeatAdvertisesEveryHostTool(t *testing.T) {
	seat, err := os.ReadFile("sidecar/seat.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(seat)
	if strings.Contains(src, `.filter((t) => t.name.startsWith("jevons_"))`) {
		t.Fatal("seat.ts must not filter host tools to jevons_* — every discovered tool is model-visible (T871.1)")
	}
	if !strings.Contains(src, "const hosted = (host ?? []).map((t) => jevonsTool(") {
		t.Fatal("seat.ts must map every host tool onto an AgentTool")
	}
}

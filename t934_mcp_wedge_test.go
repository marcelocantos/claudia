// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// jevons 🎯T934: a stdio server that is alive but never answers initialize
// is restarted by the host, and the next initialize reaches the new process.
func TestT934WedgedStdioServerIsRestartedAndAnswersNext(t *testing.T) {
	prevTimeout, prevGrace := mcpStdioRequestTimeout, mcpStdioWedgeGrace
	mcpStdioRequestTimeout, mcpStdioWedgeGrace = 300*time.Millisecond, 0
	t.Cleanup(func() { mcpStdioRequestTimeout, mcpStdioWedgeGrace = prevTimeout, prevGrace })
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	marker := filepath.Join(t.TempDir(), "wedged-once")
	url := h.Attach([]MCPServer{{Name: "wedgy", Command: bin, Env: map[string]string{"MCPSTDIO_WEDGE_ONCE": marker}}})[0].URL

	before := mcpStdioWedgeRestarts.Load()
	if r := postMCP(t, url, fixtureInit); errorCode(t, r) != mcpStdioTimeoutCode {
		t.Fatalf("a wedged server's initialize: %v, want the timeout error", r.body)
	}
	if n := mcpStdioWedgeRestarts.Load() - before; n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}
	// The fresh process gets the ordinary deadline: the short one only had
	// to catch the wedge, and a loaded host can take longer than it to start
	// a process at all.
	mcpStdioRequestTimeout = prevTimeout
	if r := postMCP(t, url, fixtureInit); r.body["result"] == nil {
		t.Fatalf("initialize after the restart: %v", r.body)
	}
}

// A server that wedges every time is restarted only up to the bound.
func TestT934WedgeRestartsAreBounded(t *testing.T) {
	prevTimeout, prevGrace := mcpStdioRequestTimeout, mcpStdioWedgeGrace
	mcpStdioRequestTimeout, mcpStdioWedgeGrace = 200*time.Millisecond, 0
	t.Cleanup(func() { mcpStdioRequestTimeout, mcpStdioWedgeGrace = prevTimeout, prevGrace })
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	url := h.Attach([]MCPServer{{Name: "broken", Command: bin, Env: map[string]string{"MCPSTDIO_WEDGE_ALWAYS": "1"}}})[0].URL
	before := mcpStdioWedgeRestarts.Load()
	for range mcpStdioMaxWedgeRestarts + 2 {
		postMCP(t, url, fixtureInit)
	}
	if n := mcpStdioWedgeRestarts.Load() - before; n != mcpStdioMaxWedgeRestarts {
		t.Fatalf("restarts = %d, want the bound %d", n, mcpStdioMaxWedgeRestarts)
	}
}

// A seat's launch names the servers that listed no tools, so its caller can
// say so before briefing it.
func TestT934LaunchReportsServersThatListedNoTools(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req["method"] {
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]any{"tools": []any{map[string]any{"name": "ping", "inputSchema": map[string]any{"type": "object"}}}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
		}
	}))
	t.Cleanup(good.Close)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(dead.Close)
	_, _, _, unavailable := hostToolsReport(context.Background(), []MCPServer{
		{Name: "bullseye-t934", Type: "http", URL: dead.URL},
		{Name: "jevons-t934", Type: "http", URL: good.URL},
	})
	if !slices.Equal(unavailable, []string{"bullseye-t934"}) {
		t.Fatalf("unavailable = %v, want only the server that answered nothing", unavailable)
	}
	a := &Agent{}
	start := &agentStart{HostMCPUnavailable: unavailable}
	a.hostMCPUnavailable = append([]string(nil), start.HostMCPUnavailable...)
	if got := a.HostMCPUnavailable(); !slices.Equal(got, unavailable) {
		t.Fatalf("Agent.HostMCPUnavailable = %v", got)
	}
}

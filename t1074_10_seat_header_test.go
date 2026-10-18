// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestT107410RunToolNamesTheSeatOnTheToolCall(t *testing.T) {
	var callSeat, initSeat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			initSeat = r.Header.Get(jevonsSeatHeader)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
		case "tools/list":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
				`{"name":"fixture_tool","description":"fixture","inputSchema":{"type":"object"}}]}}`)
		case "tools/call":
			callSeat = r.Header.Get(jevonsSeatHeader)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"named"}]}}`)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	_, routes := hostTools(context.Background(), []MCPServer{{Name: "fixture", Type: "http", URL: srv.URL}})
	ctrl := &ompControl{seat: "jevons", toolServers: routes}
	if got := ctrl.runTool("fixture_tool", "call-1", "{}"); got != "named" {
		t.Fatalf("runTool = %q", got)
	}
	if callSeat != "jevons" {
		t.Fatalf("tools/call seat header = %q", callSeat)
	}
	if initSeat != "" {
		t.Fatalf("initialize seat header = %q", initSeat)
	}
	if got := CallMCPTool(srv.URL, "fixture_tool", "{}"); got != "named" {
		t.Fatalf("CallMCPTool = %q", got)
	}
	if callSeat != "" {
		t.Fatalf("seat-less call header = %q", callSeat)
	}
}

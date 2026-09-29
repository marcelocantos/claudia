// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var providerToolPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// 🎯T146: jevons exposes `self_test.list`; advertised as is, Anthropic
// refused every turn of every sidecar seat. The advertised name is safe for
// every provider, and a call under it reaches the server by its own name.
func TestT146HostToolNamesAreProviderSafeAndRouteBack(t *testing.T) {
	var called []string
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
		case "tools/list":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+
				`{"name":"self_test.list","inputSchema":{"type":"object"}},`+
				`{"name":"jevons_agent_send","inputSchema":{"type":"object"}},`+
				`{"name":"...","inputSchema":{"type":"object"}},`+
				`{"name":"`+strings.Repeat("x", 90)+`","inputSchema":{"type":"object"}}]}}`)
		case "tools/call":
			called = append(called, req.Params.Name)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`)
		}
	}))
	defer srv.Close()

	raw, routes, names := hostToolsNamed(context.Background(), []MCPServer{{Name: "jevons", Type: "http", URL: srv.URL}})
	var tools []hostJevonsTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	var advertised []string
	for _, tool := range tools {
		if !providerToolPattern.MatchString(tool.Name) {
			t.Fatalf("advertised %q, which a provider rejects", tool.Name)
		}
		advertised = append(advertised, tool.Name)
	}
	if strings.Join(advertised, ",") != "self_test_list,jevons_agent_send,"+strings.Repeat("x", 64) {
		t.Fatalf("advertised = %v", advertised)
	}
	ctrl := &ompControl{toolServers: routes, toolNames: names}
	if got := ctrl.runTool("self_test_list", "c1", "{}"); got != "ok" {
		t.Fatalf("runTool = %q", got)
	}
	ctrl.runTool("jevons_agent_send", "c2", "{}")
	if strings.Join(called, ",") != "self_test.list,jevons_agent_send" {
		t.Fatalf("server was called as %v, want its own names", called)
	}
}

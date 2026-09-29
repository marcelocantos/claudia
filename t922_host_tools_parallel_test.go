// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// Jevons 🎯T922: a seat's host tool lists are asked of every MCP server at
// once and reused, so a launch never waits one timeout per silent server —
// four of them made a sidecar launch take ~25 s on 2026-09-29.
func TestT922HostToolListsAreFetchedConcurrentlyAndReused(t *testing.T) {
	const n = 3
	var arrived sync.WaitGroup
	arrived.Add(n)
	var requests atomic.Int32
	servers := make([]MCPServer, n)
	for i := range n {
		name := []string{"alpha", "beta", "gamma"}[i]
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			requests.Add(1)
			// Each answers only once all have been asked: one server at a
			// time would wait here for ever.
			arrived.Done()
			arrived.Wait()
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"`+name+`_tool","inputSchema":{"type":"object"}}]}}`)
		}))
		t.Cleanup(srv.Close)
		servers[i] = MCPServer{Name: name, Type: "http", URL: srv.URL}
		t.Cleanup(func() { hostToolsCache.Delete(srv.URL) })
	}
	_, routes, _ := hostToolsNamed(context.Background(), servers)
	for _, want := range []string{"alpha_tool", "beta_tool", "gamma_tool"} {
		if routes[want] == "" {
			t.Fatalf("%s not advertised: %v", want, routes)
		}
	}
	// A second launch within the TTL asks nobody.
	before := requests.Load()
	if _, routes2, _ := hostToolsNamed(context.Background(), servers); len(routes2) != n {
		t.Fatalf("second launch routes = %v", routes2)
	}
	if requests.Load() != before {
		t.Fatalf("the second launch asked the servers again (%d requests)", requests.Load()-before)
	}
}

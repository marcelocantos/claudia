// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Tiny stdio MCP server for 🎯T2.16 hermetic initialize timing.
//
// Like the mcpbridge-fronted servers the host really runs (jevons 🎯T928),
// it answers nothing before initialize and silently drops every method it
// does not know. After initialize it also answers ping at once and sleep
// (params.ms) after a delay, on its own goroutine, so replies can arrive
// out of request order.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	reply := func(id any, result map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	initialized := false
	for sc.Scan() {
		var req map[string]any
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		switch {
		case method == "initialize":
			initialized = true
			reply(req["id"], map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mcpstdio-fixture", "version": "0"},
			})
		case !initialized:
			// Dropped, as a pre-initialize request is by the real servers.
		case method == "ping":
			reply(req["id"], map[string]any{})
		case method == "sleep":
			params, _ := req["params"].(map[string]any)
			ms, _ := params["ms"].(float64)
			id := req["id"]
			go func() {
				time.Sleep(time.Duration(ms) * time.Millisecond)
				reply(id, map[string]any{"slept": ms})
			}()
		}
	}
}

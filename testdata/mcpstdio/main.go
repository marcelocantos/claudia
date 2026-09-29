// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Tiny stdio MCP server for 🎯T2.16 hermetic initialize timing.
//
// It behaves like mcpbridge before it is initialized (jevons 🎯T928): a
// request that arrives before initialize is queued, not answered, until
// initialize arrives. tools/list answers one tool; "never" is never
// answered; anything else is ignored.
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	initialized := false
	var queued []map[string]any
	answer := func(req map[string]any) {
		switch req["method"] {
		case "tools/list":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req["id"],
				"result": map[string]any{
					"tools": []any{map[string]any{"name": "fixture_tool", "inputSchema": map[string]any{"type": "object"}}},
				},
			})
		}
	}
	for sc.Scan() {
		var req map[string]any
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		if method != "initialize" {
			if !initialized {
				queued = append(queued, req)
				continue
			}
			answer(req)
			continue
		}
		_ = enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mcpstdio-fixture", "version": "0"},
			},
		})
		if !initialized {
			initialized = true
			for _, q := range queued {
				answer(q)
			}
			queued = nil
		}
	}
}

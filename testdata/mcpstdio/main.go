// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Tiny stdio MCP server for 🎯T2.16 hermetic initialize timing.
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req map[string]any
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		if method != "initialize" {
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
	}
}

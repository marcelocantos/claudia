// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
)

const mcpReplyLimit = 4 << 20

// readMCPTerminal never turns a progress notification, truncated body or
// another invocation's reply into a durable operation's successful outcome.
func readMCPTerminal(resp *http.Response, expected json.RawMessage) ([]byte, error) {
	limited := &io.LimitedReader{R: resp.Body, N: mcpReplyLimit + 1}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, err := io.ReadAll(limited)
		if err != nil {
			return nil, err
		}
		if len(raw) > mcpReplyLimit {
			return nil, fmt.Errorf("MCP response exceeds %d bytes", mcpReplyLimit)
		}
		terminal, err := matchingMCPTerminal(raw, expected)
		if err != nil {
			return nil, err
		}
		if !terminal {
			return nil, fmt.Errorf("MCP response ended without matching terminal reply")
		}
		return raw, nil
	}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), mcpReplyLimit+1)
	var data []string
	for scanner.Scan() {
		if limited.N == 0 {
			return nil, fmt.Errorf("MCP response exceeds %d bytes", mcpReplyLimit)
		}
		line := scanner.Text()
		if line != "" {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(value, " "))
			}
			continue
		}
		if len(data) == 0 {
			continue
		}
		raw := []byte(strings.Join(data, "\n"))
		data = nil
		terminal, err := matchingMCPTerminal(raw, expected)
		if err != nil {
			return nil, err
		}
		if terminal {
			return raw, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("MCP stream ended without matching terminal reply")
}

func matchingMCPTerminal(raw, expected json.RawMessage) (bool, error) {
	var response struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return false, fmt.Errorf("invalid MCP response: %w", err)
	}
	if response.Version != "2.0" {
		return false, fmt.Errorf("invalid MCP JSON-RPC version")
	}
	if len(response.ID) == 0 && response.Method != "" && len(response.Result) == 0 && len(response.Error) == 0 {
		return false, nil // A notification is not the invocation's answer.
	}
	var got, want any
	if json.Unmarshal(response.ID, &got) != nil || json.Unmarshal(expected, &want) != nil || !reflect.DeepEqual(got, want) {
		return false, fmt.Errorf("MCP terminal response identity mismatch")
	}
	if (len(response.Result) == 0) == (len(response.Error) == 0) || response.Method != "" || bytes.Equal(response.ID, []byte("null")) {
		return false, fmt.Errorf("invalid MCP terminal response")
	}
	return true, nil
}

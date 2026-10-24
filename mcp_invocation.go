// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"
)

// MCPInvocationMetaKey names Claudia's provider/consumer-neutral identity in
// tools/call params._meta. It is correlation data, not an authentication claim.
const MCPInvocationMetaKey = "io.github.marcelocantos.claudia/invocation"

// MCPInvocationIdentity survives connection changes and provider switches.
// Servers must scope tool-call IDs by both seat and session: providers may
// reuse a call ID in a different conversation. The JSON-RPC id is ToolCallID
// unchanged; transport Mcp-Session-Id is not this durable session identity.
type MCPInvocationIdentity struct {
	Seat       string `json:"seat"`
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
}

const mcpToolCallTimeout = 30 * time.Second

type mcpInvocation struct {
	url, name, args string
	identity        MCPInvocationIdentity
	durable         bool
}

type ompToolCall struct {
	ctx    context.Context
	cancel context.CancelFunc
	turnID string
}

func (c *ompControl) beginToolCall(turnID string) *ompToolCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if turnID == "" {
		turnID = c.turnID
	}
	ctx, cancel := context.WithCancel(context.Background())
	call := &ompToolCall{ctx: ctx, cancel: cancel, turnID: turnID}
	if c.toolCalls == nil {
		c.toolCalls = make(map[*ompToolCall]struct{})
	}
	c.toolCalls[call] = struct{}{}
	if c.toolsClosed {
		cancel()
	}
	return call
}

func (c *ompControl) finishToolCall(call *ompToolCall) {
	call.cancel()
	c.mu.Lock()
	delete(c.toolCalls, call)
	c.mu.Unlock()
}

func (c *ompControl) closeToolCalls() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolsClosed = true
	for call := range c.toolCalls {
		call.cancel()
	}
}

func (c *ompControl) cancelToolTurn(turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for call := range c.toolCalls {
		if turnID == "" || call.turnID == turnID {
			call.cancel()
		}
	}
}

func (c *ompControl) runToolContext(ctx context.Context, name, callID, args string) string {
	c.mu.Lock()
	url, routed := c.toolServers[name]
	if original, renamed := c.toolNames[name]; renamed {
		name = original
	}
	call := &mcpInvocation{url: url, name: name, args: args,
		identity: MCPInvocationIdentity{Seat: c.seat, SessionID: c.sessionID, ToolCallID: callID}}
	for _, server := range c.mcpServers {
		if server.URL == url && slices.Contains(server.DurableTools, name) {
			call.durable = true
			break
		}
	}
	c.mu.Unlock()
	if routed {
		return invokeMCPTool(ctx, call)
	}
	return runOMPTool(name, callID, args)
}

func cancelMCPInvocation(url, session, seat string, id any) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": id, "reason": "caller stopped waiting; effect outcome may be unknown"},
	})
	if err != nil {
		slog.Warn("MCP cancellation could not be encoded", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostToolsBudget)
	defer cancel()
	if _, _, cause, err := mcpPost(ctx, url, session, body, seat); cause != "" {
		slog.Warn("MCP cancellation not acknowledged; effect outcome remains unknown", "cause", cause, "error", err)
	}
}

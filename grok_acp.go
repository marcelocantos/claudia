// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// grokACPClient is a minimal ACP (Agent Client Protocol) client over
// JSON-RPC 2.0 lines on a child process's stdin/stdout. It drives
// Grok Build's `grok agent stdio` session surface.
//
// See https://agentclientprotocol.com and Grok Build agent-mode docs.
type grokACPClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan acpRPCMessage
	closed  bool

	sessionID string
	onEvent   func(Event)
	onClose   func()

	// promptID is the in-flight session/prompt request id; when its
	// response arrives, a terminal assistant event is published.
	promptID int64
}

type acpRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// startGrokACP spawns bin (typically the grok CLI) with ACP stdio args,
// performs initialize + session setup, and returns a ready client.
//
// model is optional. sessionID, if non-empty and loadSession is available,
// tries session/load first and falls back to session/new on failure.
func startGrokACP(bin string, workDir, model, sessionID string, requireResume bool, mcpServers []any, onEvent func(Event), onClose func()) (*grokACPClient, error) {
	args := []string{"agent", "--always-approve", "stdio"}
	if model != "" {
		// Flags on `grok agent` apply to every mode; pass before stdio.
		args = []string{"agent", "--always-approve", "--model", model, "stdio"}
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = workDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("grok acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("grok acp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("grok acp stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start grok acp: %w", err)
	}

	c := &grokACPClient{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		pending:   make(map[int64]chan acpRPCMessage),
		onEvent:   onEvent,
		onClose:   onClose,
		sessionID: sessionID,
	}

	go c.drainStderr()
	go c.readLoop()

	if err := c.initialize(); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openSession(workDir, sessionID, requireResume, mcpServers); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *grokACPClient) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *grokACPClient) drainStderr() {
	sc := bufio.NewScanner(c.stderr)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		slog.Debug("grok acp stderr", "line", sc.Text())
	}
}

func (c *grokACPClient) readLoop() {
	defer func() {
		c.mu.Lock()
		c.closed = true
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if c.onClose != nil {
			c.onClose()
		}
	}()

	sc := bufio.NewScanner(c.stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg acpRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			slog.Debug("grok acp ignore non-json line", "err", err)
			continue
		}
		// Client-bound request (agent → client): auto-handle permissions.
		if msg.ID != nil && msg.Method != "" {
			c.handleServerRequest(msg)
			continue
		}
		// Response to a client request.
		if msg.ID != nil {
			c.mu.Lock()
			ch := c.pending[*msg.ID]
			if ch != nil {
				delete(c.pending, *msg.ID)
			}
			isPrompt := c.promptID == *msg.ID
			if isPrompt {
				c.promptID = 0
			}
			c.mu.Unlock()
			if isPrompt {
				c.publishPromptResult(msg)
			}
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
			continue
		}
		// Notification.
		if msg.Method != "" {
			c.handleNotification(msg)
		}
	}
}

func (c *grokACPClient) handleServerRequest(msg acpRPCMessage) {
	switch msg.Method {
	case "session/request_permission":
		// Auto-approve: unattended embedding posture (mirrors --always-approve).
		_ = c.reply(msg.ID, map[string]any{
			"outcome": map[string]any{
				"outcome":  "selected",
				"optionId": "allow_always",
			},
		})
	case "fs/read_text_file", "fs/write_text_file",
		"terminal/create", "terminal/output", "terminal/release",
		"terminal/wait_for_exit", "terminal/kill":
		// Minimal client: decline rich fs/terminal. Agent still has its own tools.
		_ = c.replyError(msg.ID, -32601, "claudia grok acp client does not implement "+msg.Method)
	default:
		_ = c.replyError(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func (c *grokACPClient) handleNotification(msg acpRPCMessage) {
	switch msg.Method {
	case "session/update":
		c.handleSessionUpdate(msg.Params)
	}
}

func (c *grokACPClient) handleSessionUpdate(params json.RawMessage) {
	if c.onEvent == nil || len(params) == 0 {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Title  string `json:"title"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	raw, _ := json.Marshal(p)
	switch p.Update.SessionUpdate {
	case "agent_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		if text == "" {
			return
		}
		c.onEvent(Event{Type: "assistant", Raw: raw, Text: text})
	case "tool_call", "tool_call_update":
		c.onEvent(Event{Type: "progress", Raw: raw, ProgressType: "tool_use"})
	case "user_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		c.onEvent(Event{Type: "user", Raw: raw, Text: text})
	}
}

func (c *grokACPClient) initialize() error {
	result, err := c.request("initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "claudia",
			"version": Version,
		},
		"clientCapabilities": map[string]any{
			// Advertise nothing rich: we auto-approve permissions and
			// decline fs/terminal server requests.
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	})
	if err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	_ = result
	// Spec-adjacent initialized notification (harmless if ignored).
	_ = c.notify("notifications/initialized", map[string]any{})
	return nil
}

func (c *grokACPClient) openSession(workDir, preferSessionID string, requireResume bool, mcpServers []any) error {
	if preferSessionID != "" {
		err := c.loadSession(preferSessionID, workDir, mcpServers)
		if err == nil {
			return nil
		}
		// FAIL-CLOSED LOAD: when the caller marked this id as an existing
		// conversation (RequireResume), refusing to load it must be an
		// error — silently minting a fresh session here is how a
		// conversation gets lost (the caller would adopt the new id and
		// orphan the old one). Only an unmaterialized id may fall through
		// to session/new (the legitimate first-launch path).
		if requireResume {
			return fmt.Errorf("acp session/load %s: %w — existing conversation; refusing to mint a replacement session", preferSessionID, err)
		}
		slog.Debug("grok acp session/load failed for unmaterialized id; creating new session", "err", err, "session", preferSessionID)
	}
	if mcpServers == nil {
		mcpServers = []any{}
	}
	result, err := c.request("session/new", map[string]any{
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return fmt.Errorf("acp session/new: %w", err)
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		return fmt.Errorf("acp session/new decode: %w", err)
	}
	if out.SessionID == "" {
		return fmt.Errorf("acp session/new: empty sessionId")
	}
	c.mu.Lock()
	c.sessionID = out.SessionID
	c.mu.Unlock()
	return nil
}

func (c *grokACPClient) loadSession(sessionID, workDir string, mcpServers []any) error {
	if mcpServers == nil {
		mcpServers = []any{}
	}
	result, err := c.request("session/load", map[string]any{
		"sessionId":  sessionID,
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return err
	}
	// Some agents echo sessionId; others return empty result on success.
	var out struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(result, &out)
	c.mu.Lock()
	if out.SessionID != "" {
		c.sessionID = out.SessionID
	} else {
		c.sessionID = sessionID
	}
	c.mu.Unlock()
	return nil
}

// Prompt starts a session/prompt turn without blocking for completion.
// Streaming session/update notifications are delivered via onEvent; the
// prompt's JSON-RPC result publishes a terminal assistant event so
// WaitForResponse can resolve. Callers typically Send then WaitForResponse.
func (c *grokACPClient) Prompt(text string) error {
	c.mu.Lock()
	sid := c.sessionID
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("grok acp: client closed")
	}
	if sid == "" {
		c.mu.Unlock()
		return fmt.Errorf("grok acp: no session")
	}
	if c.promptID != 0 {
		c.mu.Unlock()
		return fmt.Errorf("grok acp: prompt already in flight")
	}
	id := atomic.AddInt64(&c.nextID, 1)
	c.promptID = id
	// No pending channel: completion is handled in readLoop via promptID.
	c.mu.Unlock()

	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	})
}

func (c *grokACPClient) publishPromptResult(msg acpRPCMessage) {
	if msg.Error != nil {
		if c.onEvent != nil {
			raw, _ := json.Marshal(msg.Error)
			c.onEvent(Event{
				Type:       "assistant",
				Raw:        raw,
				Text:       msg.Error.Message,
				StopReason: "end_turn",
			})
		}
		return
	}
	stopReason := "end_turn"
	var usage Usage
	var meta struct {
		StopReason string `json:"stopReason"`
		Meta       *struct {
			InputTokens      int `json:"inputTokens"`
			OutputTokens     int `json:"outputTokens"`
			CachedReadTokens int `json:"cachedReadTokens"`
		} `json:"_meta"`
	}
	if len(msg.Result) > 0 {
		if err := json.Unmarshal(msg.Result, &meta); err == nil {
			if meta.StopReason != "" {
				stopReason = meta.StopReason
			}
			if meta.Meta != nil {
				usage = Usage{
					InputTokens:          meta.Meta.InputTokens,
					OutputTokens:         meta.Meta.OutputTokens,
					CacheReadInputTokens: meta.Meta.CachedReadTokens,
				}
			}
		}
	}
	// Normalise to WaitForResponse terminal reasons.
	switch stopReason {
	case "end_turn", "stop_sequence", "max_tokens":
	case "cancelled", "refusal":
		stopReason = "end_turn"
	default:
		stopReason = "end_turn"
	}
	if c.onEvent != nil {
		raw := msg.Result
		if len(raw) == 0 {
			raw, _ = json.Marshal(map[string]any{"stopReason": stopReason})
		}
		c.onEvent(Event{
			Type:       "assistant",
			Raw:        raw,
			StopReason: stopReason,
			Usage:      usage,
		})
	}
}

// Cancel sends session/cancel for the active session (interrupt).
func (c *grokACPClient) Cancel() error {
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid == "" {
		return nil
	}
	return c.notify("session/cancel", map[string]any{"sessionId": sid})
}

func (c *grokACPClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.stdin.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
}

func (c *grokACPClient) nextReqID() int64 {
	return atomic.AddInt64(&c.nextID, 1)
}

func (c *grokACPClient) request(method string, params any) (json.RawMessage, error) {
	id := c.nextReqID()
	ch := make(chan acpRPCMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("grok acp: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if pj, err := json.Marshal(params); err == nil {
		slog.Debug("grok acp request", "method", method, "params", string(pj))
	}
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	resp, ok := <-ch
	if !ok {
		return nil, fmt.Errorf("grok acp: connection closed waiting for %s", method)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("acp %s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

func (c *grokACPClient) notify(method string, params any) error {
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *grokACPClient) reply(id *int64, result any) error {
	if id == nil {
		return nil
	}
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      *id,
		"result":  result,
	})
}

func (c *grokACPClient) replyError(id *int64, code int, message string) error {
	if id == nil {
		return nil
	}
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      *id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (c *grokACPClient) write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("grok acp: client closed")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

// acpMCPServers converts a Claude-style .mcp.json file (the format
// Config.MCPConfig points at: {"mcpServers":{name:{...}}}) into the ACP
// session mcpServers array. grok agent stdio loads MCP servers ONLY from
// this session parameter — it reads neither ~/.grok/config.toml nor a
// cwd .mcp.json — so an agent launched without it has no tools at all.
// HTTP entries map to ACP's http variant; stdio entries map to the
// command variant. A missing/unreadable/empty file yields nil (no MCP).
func acpMCPServers(mcpConfigPath string) []any {
	if mcpConfigPath == "" {
		return nil
	}
	data, err := os.ReadFile(mcpConfigPath)
	if err != nil {
		return nil
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Warn("grok acp: bad mcp config; launching without MCP", "path", mcpConfigPath, "err", err)
		return nil
	}
	var out []any
	for name, srv := range cfg.MCPServers {
		switch {
		case srv.URL != "":
			out = append(out, map[string]any{
				"type":    "http",
				"name":    name,
				"url":     srv.URL,
				"headers": []any{},
			})
		case srv.Command != "":
			envs := []any{}
			for k, v := range srv.Env {
				envs = append(envs, map[string]any{"name": k, "value": v})
			}
			args := srv.Args
			if args == nil {
				args = []string{}
			}
			out = append(out, map[string]any{
				"name":    name,
				"command": srv.Command,
				"args":    args,
				"env":     envs,
			})
		}
	}
	return out
}

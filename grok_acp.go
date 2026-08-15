// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// grokACPClient is a minimal ACP (Agent Client Protocol) client over
// JSON-RPC 2.0. Transport is either:
//
//   - stdio pipes to a parent-owned `grok agent stdio` child (default), or
//   - WebSocket to a detached `grok agent serve` process (connect-mode;
//     🎯T40 process durability — agent outlives the consumer).
//
// See https://agentclientprotocol.com and Grok Build agent-mode docs.
type grokACPClient struct {
	// stdio transport (default)
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	// connect-mode WebSocket transport (mutually exclusive with stdio pipes)
	ws       *websocket.Conn
	wsCtx    context.Context
	wsCancel context.CancelFunc

	// ownsProcess: when true, Close kills cmd (stdio child or serve we
	// spawned). Reattach to an external serve also sets ownsProcess so
	// Agent.Stop tears down the durable process.
	ownsProcess bool
	// connectURL / connectPID describe the durable serve endpoint when
	// using connect-mode (empty/0 for stdio).
	connectURL string
	connectPID int

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
// model is optional. sessionID and requireResume control resume behaviour;
// see openSession. mcpServers are attached on session/new (Grok CLI
// attaches tools only there — not on session/load).
// grokACPArgs builds the argv `grok agent` is launched with, up to and
// including the mode word. Connect mode appends its own --bind/--secret,
// which are minted per launch and so cannot live here.
//
// Flags on `grok agent` apply to every mode and must precede the mode
// word — a --model after "stdio" is parsed as a flag of the mode, not the
// agent, and is ignored.
func grokACPArgs(model string, connect bool) []string {
	args := []string{"agent", "--always-approve"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if connect {
		return append(args, "serve")
	}
	return append(args, "stdio")
}

func startGrokACP(bin string, workDir, model, sessionID string, requireResume bool, mcpServers []any, onEvent func(Event), onClose func()) (*grokACPClient, error) {
	cmd := exec.Command(bin, grokACPArgs(model, false)...)
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
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		ownsProcess: true,
		pending:     make(map[int64]chan acpRPCMessage),
		onEvent:     onEvent,
		onClose:     onClose,
		sessionID:   sessionID,
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

// ConnectURL returns the durable serve WebSocket URL in connect-mode, or "".
func (c *grokACPClient) ConnectURL() string { return c.connectURL }

// ConnectPID returns the durable serve PID in connect-mode, or 0.
func (c *grokACPClient) ConnectPID() int { return c.connectPID }

func (c *grokACPClient) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *grokACPClient) drainStderr() {
	if c.stderr == nil {
		return
	}
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

	if c.ws != nil {
		c.readLoopWS()
		return
	}
	if c.stdout == nil {
		return
	}
	sc := bufio.NewScanner(c.stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		c.dispatchMessage(line)
	}
}

func (c *grokACPClient) readLoopWS() {
	for {
		_, data, err := c.ws.Read(c.wsCtx)
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		// Serve may send one JSON object per frame; strip optional newline.
		line := bytesTrimSpace(data)
		if len(line) == 0 {
			continue
		}
		c.dispatchMessage(line)
	}
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		last := b[len(b)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

func (c *grokACPClient) dispatchMessage(line []byte) {
	var msg acpRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.Debug("grok acp ignore non-json line", "err", err)
		return
	}
	// Client-bound request (agent → client): auto-handle permissions.
	if msg.ID != nil && msg.Method != "" {
		c.handleServerRequest(msg)
		return
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
		return
	}
	// Notification.
	if msg.Method != "" {
		c.handleNotification(msg)
	}
}

func (c *grokACPClient) handleServerRequest(msg acpRPCMessage) {
	switch msg.Method {
	case "session/request_permission":
		// Auto-approve for unattended embedding. Must pick an optionId that
		// appears in the request: Grok offers tool-specific IDs for shell
		// (e.g. allow_always_bash) and rejects unknown ones with
		// "unknown permission option for tool run_terminal_command".
		optionID := selectPermissionOptionID(msg.Params)
		_ = c.reply(msg.ID, map[string]any{
			"outcome": map[string]any{
				"outcome":  "selected",
				"optionId": optionID,
			},
		})
	case "fs/read_text_file", "fs/write_text_file",
		"terminal/create", "terminal/output", "terminal/release",
		"terminal/wait_for_exit", "terminal/kill":
		// Minimal client: decline rich fs/terminal. Agent still has its own tools.
		_ = c.replyError(msg.ID, -32601, "claudia grok acp client does not implement "+msg.Method)
	default:
		slog.Warn("grok acp unhandled server request", "method", msg.Method, "params", string(msg.Params))
		_ = c.replyError(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

// selectPermissionOptionID chooses an allow option from a
// session/request_permission params blob. Preference: generic
// allow_always, then any allow_always_* (bash/mcp/domain), then
// allow_once, then any other allow_*. Empty or unparseable params fall
// back to allow_always (legacy Grok shape).
func selectPermissionOptionID(params json.RawMessage) string {
	ids := permissionOptionIDs(params)
	if len(ids) == 0 {
		return "allow_always"
	}
	has := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		has[id] = struct{}{}
	}
	if _, ok := has["allow_always"]; ok {
		return "allow_always"
	}
	// Tool-scoped always grants (shell, MCP, domain, …).
	for _, id := range ids {
		if strings.HasPrefix(id, "allow_always") {
			return id
		}
	}
	if _, ok := has["allow_once"]; ok {
		return "allow_once"
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "allow_") {
			return id
		}
	}
	// Offered only rejects or unknown kinds — still must pick something
	// from the list so Grok does not error on a foreign optionId.
	return ids[0]
}

func permissionOptionIDs(params json.RawMessage) []string {
	if len(params) == 0 {
		return nil
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
		} `json:"options"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	var ids []string
	for _, opt := range p.Options {
		if opt.OptionID != "" {
			ids = append(ids, opt.OptionID)
		}
	}
	return ids
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
	// Probe only the fields needed for routing; keep Event.Raw as the
	// original params so tool_call rawInput/arguments survive for consumers
	// (jevons activity strip 🎯T64). Re-marshalling a narrow struct dropped
	// rawInput and broke arg summaries.
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
	switch p.Update.SessionUpdate {
	case "agent_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		if text == "" {
			return
		}
		c.onEvent(Event{Type: "assistant", Raw: params, Text: text})
	case "tool_call", "tool_call_update":
		// Pass params through unchanged so rawInput is preserved.
		c.onEvent(Event{Type: "progress", Raw: params, ProgressType: "tool_use"})
	case "user_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		c.onEvent(Event{Type: "user", Raw: params, Text: text})
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
	if mcpServers == nil {
		mcpServers = []any{}
	}
	tooled := len(mcpServers) > 0

	// Grok CLI bug (not ACP): ACP requires Agents to reconnect MCP on
	// session/load and session/resume, but Grok attaches mcpServers only
	// on session/new and silently ignores them on session/load (observed
	// 2026-07-18 in jevons: resumed overseer ran with zero tools). See
	// docs/grok-acp-session.md § Resume and MCP.
	//
	// Policy when tools are configured: always session/new with mcpServers
	// (rotation). Same-id load cannot keep tools under today's Grok CLI.
	// Hosts that need continuity inject a recap after Start; Registry
	// already persists SessionID when it changes after launch.
	// RequireResume is overridden here — keeping the old id would mean a
	// toolless agent, which is worse than a rotated id.
	if preferSessionID != "" && tooled {
		slog.Info("grok acp: rotating session for tools",
			"prior_session", preferSessionID,
			"require_resume", requireResume,
			"reason", "Grok CLI ignores mcpServers on session/load")
		return c.createSession(workDir, mcpServers)
	}

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
	return c.createSession(workDir, mcpServers)
}

func (c *grokACPClient) createSession(workDir string, mcpServers []any) error {
	if mcpServers == nil {
		mcpServers = []any{}
	}
	// yoloMode is Grok's ACP always-approve switch for the session
	// (belt-and-suspenders with CLI --always-approve). See Grok agent-mode docs.
	result, err := c.request("session/new", map[string]any{
		"cwd":        workDir,
		"mcpServers": mcpServers,
		"_meta":      map[string]any{"yoloMode": true},
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
		"_meta":      map[string]any{"yoloMode": true},
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
// Clears the local prompt-in-flight gate immediately so a subsequent
// Prompt is not stuck on "prompt already in flight" when the peer never
// returns a result for the cancelled id (half-dead serve / lost WS).
func (c *grokACPClient) Cancel() error {
	c.mu.Lock()
	sid := c.sessionID
	c.promptID = 0
	c.mu.Unlock()
	if sid == "" {
		return nil
	}
	return c.notify("session/cancel", map[string]any{"sessionId": sid})
}

// promptInFlight reports whether a session/prompt is awaiting completion.
func (c *grokACPClient) promptInFlight() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.promptID != 0
}

func (c *grokACPClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	owns := c.ownsProcess
	cmd := c.cmd
	pid := c.connectPID
	ws := c.ws
	wsCancel := c.wsCancel
	stdin := c.stdin
	c.mu.Unlock()

	if wsCancel != nil {
		wsCancel()
	}
	if ws != nil {
		_ = ws.Close(websocket.StatusNormalClosure, "bye")
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if !owns {
		return
	}
	// Kill the durable serve by PID when we reattached without cmd, or
	// the stdio/serve child we still hold as cmd.
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return
	}
	if pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
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
	if c.ws != nil {
		// One JSON-RPC message per WebSocket text frame.
		return c.ws.Write(c.wsCtx, websocket.MessageText, b)
	}
	if c.stdin == nil {
		return fmt.Errorf("grok acp: no transport")
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

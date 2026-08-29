// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
)

// ErrCursorResumeDenied is returned when session/load failed for a
// conversation that already has store.db (or RequireResume). Callers
// must not retry Launch — a second client stacks a writer on the same
// store (🎯T541.1).
var ErrCursorResumeDenied = errors.New("existing conversation; refusing to mint a replacement session")

// IsCursorResumeDenied reports whether err is (or wraps) ErrCursorResumeDenied.
func IsCursorResumeDenied(err error) bool {
	return errors.Is(err, ErrCursorResumeDenied)
}

// cursorACPClient is a minimal ACP client over JSON-RPC 2.0 stdio to
// `agent acp`. See https://cursor.com/docs/cli/acp and
// https://agentclientprotocol.com.
type cursorACPClient struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	ownsProcess bool

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan acpRPCMessage
	closed  bool

	sessionID string
	onEvent   func(Event)
	onClose   func()
	promptID  int64
}

// cursorACPArgs builds the argv after the agent binary. Root flags must
// precede the `acp` mode word — a --model after "acp" is ignored.
// Auth uses the real agent login Keychain, or CURSOR_API_KEY in the
// process environment (Cursor reads it without a hermetic HOME).
func cursorACPArgs(model string) []string {
	args := []string{"--force", "--trust", "--approve-mcps"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "acp")
}

func startCursorACP(bin string, workDir, model, sessionID string, requireResume bool, mcpServers []any, extraEnv []string, onEvent func(Event), onClose func()) (*cursorACPClient, error) {
	cmd := exec.Command(bin, cursorACPArgs(model)...)
	cmd.Dir = workDir
	if len(extraEnv) > 0 {
		cmd.Env = appendEnv(nil, extraEnv)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("cursor acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("cursor acp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("cursor acp stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start cursor acp: %w", err)
	}

	c := &cursorACPClient{
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
	if err := c.authenticate(); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openSession(workDir, sessionID, requireResume, mcpServers); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *cursorACPClient) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *cursorACPClient) drainStderr() {
	if c.stderr == nil {
		return
	}
	sc := bufio.NewScanner(c.stderr)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		slog.Debug("cursor acp stderr", "line", sc.Text())
	}
}

func (c *cursorACPClient) readLoop() {
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
	if c.stdout == nil {
		return
	}
	sc := newACPLineScanner(c.stdout)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		c.dispatchMessage(line)
	}
	logACPScanErr("cursor", sc.Err())
}

func (c *cursorACPClient) dispatchMessage(line []byte) {
	var msg acpRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.Debug("cursor acp ignore non-json line", "err", err)
		return
	}
	if msg.ID != nil && msg.Method != "" {
		c.handleServerRequest(msg)
		return
	}
	if msg.ID != nil {
		c.mu.Lock()
		ch := c.pending[*msg.ID]
		if ch != nil {
			delete(c.pending, *msg.ID)
		}
		isPrompt := c.promptID == *msg.ID
		promptID := c.promptID
		sessionID := c.sessionID
		if isPrompt {
			c.promptID = 0
		}
		c.mu.Unlock()
		if isPrompt {
			c.publishPromptResult(msg, sessionID, strconv.FormatInt(promptID, 10))
		}
		if ch != nil {
			select {
			case ch <- msg:
			default:
			}
		}
		return
	}
	if msg.Method != "" {
		c.handleNotification(msg)
	}
}

func (c *cursorACPClient) handleServerRequest(msg acpRPCMessage) {
	switch msg.Method {
	case "session/request_permission":
		c.mu.Lock()
		sid, promptID := c.sessionID, c.promptID
		c.mu.Unlock()
		publishEvent(c.onEvent, acpPermissionEvent(sid, promptID, msg.Params))
		reply := permissionSelectedReply(msg.Params)
		if permissionMutatesBullseye(msg.Params) {
			slog.Warn("cursor acp refused ledger mutation",
				"reason", LedgerRefuseReason)
		}
		_ = c.reply(msg.ID, reply)
	case "cursor/ask_question":
		// Unattended: skip rather than stall the turn.
		_ = c.reply(msg.ID, map[string]any{
			"outcome": map[string]any{"outcome": "skipped", "reason": "claudia unattended client"},
		})
	case "cursor/create_plan":
		_ = c.reply(msg.ID, map[string]any{
			"outcome": map[string]any{"outcome": "accepted"},
		})
	case "fs/read_text_file", "fs/write_text_file",
		"terminal/create", "terminal/output", "terminal/release",
		"terminal/wait_for_exit", "terminal/kill":
		_ = c.replyError(msg.ID, -32601, "claudia cursor acp client does not implement "+msg.Method)
	default:
		slog.Warn("cursor acp unhandled server request", "method", msg.Method, "params", string(msg.Params))
		_ = c.replyError(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func (c *cursorACPClient) handleNotification(msg acpRPCMessage) {
	switch msg.Method {
	case "session/update":
		c.handleSessionUpdate(msg.Params)
	case "cursor/update_todos", "cursor/task", "cursor/generate_image":
		if c.onEvent != nil {
			c.onEvent(Event{Type: "progress", Raw: msg.Params, ProgressType: msg.Method})
		}
	}
}

func (c *cursorACPClient) handleSessionUpdate(params json.RawMessage) {
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
	c.mu.Lock()
	promptID := c.promptID
	clientSessionID := c.sessionID
	c.mu.Unlock()
	sessionID := clientSessionID
	if p.SessionID != "" {
		sessionID = p.SessionID
	}
	turnID := ""
	if promptID != 0 && (p.SessionID == "" || clientSessionID == "" || p.SessionID == clientSessionID) {
		turnID = strconv.FormatInt(promptID, 10)
	}
	if ev, ok := acpProgressEvent(sessionID, turnID, params); ok {
		c.onEvent(ev)
		return
	}
	probe, _ := parseACPUpdate(params)
	usage := acpUsage(probe)
	switch p.Update.SessionUpdate {
	case "agent_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		if text == "" {
			return
		}
		c.onEvent(Event{Type: "assistant", SessionID: sessionID, TurnID: turnID, Raw: params, Text: text, Usage: usage})
	case "user_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		c.onEvent(Event{Type: "user", SessionID: sessionID, TurnID: turnID, Raw: params, Text: text, Usage: usage})
	}
}

func (c *cursorACPClient) initialize() error {
	_, err := c.request("initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "claudia",
			"version": Version,
		},
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	})
	if err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	_ = c.notify("notifications/initialized", map[string]any{})
	return nil
}

func (c *cursorACPClient) authenticate() error {
	_, err := c.request("authenticate", map[string]any{
		"methodId": "cursor_login",
	})
	if err != nil {
		return fmt.Errorf("acp authenticate cursor_login: %w", err)
	}
	return nil
}

func (c *cursorACPClient) openSession(workDir, preferSessionID string, requireResume bool, mcpServers []any) error {
	if mcpServers == nil {
		mcpServers = []any{}
	}
	if preferSessionID != "" {
		err := c.loadSession(preferSessionID, workDir, mcpServers)
		if err == nil {
			return nil
		}
		// A store.db means this id already hosted a conversation.
		// session/new after a failed load stacks a second writer and
		// often dies with "client closed" (🎯T541.1).
		if requireResume || cursorACPStoreExists(preferSessionID) {
			return fmt.Errorf("acp session/load %s: %w (%w)", preferSessionID, err, ErrCursorResumeDenied)
		}
		slog.Warn("cursor acp session/load failed for unmaterialized id; creating new session", "err", err, "session", preferSessionID)
	}
	return c.createSession(workDir, mcpServers)
}

func (c *cursorACPClient) createSession(workDir string, mcpServers []any) error {
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

func (c *cursorACPClient) loadSession(sessionID, workDir string, mcpServers []any) error {
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

func (c *cursorACPClient) Prompt(text string) error {
	c.mu.Lock()
	sid := c.sessionID
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("cursor acp: client closed")
	}
	if sid == "" {
		c.mu.Unlock()
		return fmt.Errorf("cursor acp: no session")
	}
	if c.promptID != 0 {
		c.mu.Unlock()
		return fmt.Errorf("cursor acp: prompt already in flight")
	}
	id := atomic.AddInt64(&c.nextID, 1)
	c.promptID = id
	c.mu.Unlock()
	publishEvent(c.onEvent, acpPromptAcceptedEvent(sid, id))

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

func (c *cursorACPClient) publishPromptResult(msg acpRPCMessage, sessionID, turnID string) {
	if msg.Error != nil {
		if c.onEvent != nil {
			raw, _ := json.Marshal(msg.Error)
			c.onEvent(Event{
				Type:       "assistant",
				SessionID:  sessionID,
				TurnID:     turnID,
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
		Usage      *struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
		} `json:"usage"`
		Meta *struct {
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
			} else if meta.Usage != nil {
				usage = Usage{
					InputTokens:  meta.Usage.InputTokens,
					OutputTokens: meta.Usage.OutputTokens,
				}
			}
		}
	}
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
			SessionID:  sessionID,
			TurnID:     turnID,
			Raw:        raw,
			StopReason: stopReason,
			Usage:      usage,
		})
	}
}

func (c *cursorACPClient) Cancel() error {
	c.mu.Lock()
	sid := c.sessionID
	c.promptID = 0
	c.mu.Unlock()
	if sid == "" {
		return nil
	}
	return c.notify("session/cancel", map[string]any{"sessionId": sid})
}

func (c *cursorACPClient) promptInFlight() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.promptID != 0
}

func (c *cursorACPClient) Close() {
	c.mu.Lock()
	// readLoop sets closed when stdout EOFs. That is not a kill — a
	// cursor-agent that drops the ACP pipe can still hold store.db
	// (🎯T541.1 launch storm). Always signal if we own the process.
	c.closed = true
	owns := c.ownsProcess
	cmd := c.cmd
	stdin := c.stdin
	c.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if !owns {
		return
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func (c *cursorACPClient) nextReqID() int64 {
	return atomic.AddInt64(&c.nextID, 1)
}

func (c *cursorACPClient) request(method string, params any) (json.RawMessage, error) {
	id := c.nextReqID()
	ch := make(chan acpRPCMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("cursor acp: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if pj, err := json.Marshal(params); err == nil {
		slog.Debug("cursor acp request", "method", method, "params", string(pj))
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
		return nil, fmt.Errorf("cursor acp: connection closed waiting for %s", method)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("acp %s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

func (c *cursorACPClient) notify(method string, params any) error {
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *cursorACPClient) reply(id *int64, result any) error {
	if id == nil {
		return nil
	}
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      *id,
		"result":  result,
	})
}

func (c *cursorACPClient) replyError(id *int64, code int, message string) error {
	if id == nil {
		return nil
	}
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      *id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (c *cursorACPClient) write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("cursor acp: client closed")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if c.stdin == nil {
		return fmt.Errorf("cursor acp: no transport")
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

// cursorSessionPlan is everything the Cursor Session path derives from a
// start request before it launches anything. Splitting it out lets the
// request-field audit materialise the request without an agent binary.
type cursorSessionPlan struct {
	Args            []string
	WorkDir         string
	Model           string
	PreferSessionID string
	RequireResume   bool
	MCPServers      []any
	// MCPExclusive is the isolate flag from Config. Cursor has no
	// strict-mcp flag and Claudia does not rewrite project mcp.json;
	// exclusive Session MCP is ACP mcpServers only.
	MCPExclusive bool
}

func planCursorSession(req agentStartRequest) cursorSessionPlan {
	preferID := ""
	if req.Resuming || req.Config.SessionID != "" {
		preferID = req.SessionID
	}
	return cursorSessionPlan{
		Args:            cursorACPArgs(req.Config.Model),
		WorkDir:         req.WorkDir,
		Model:           req.Config.Model,
		PreferSessionID: preferID,
		RequireResume:   req.Config.RequireResume,
		MCPServers:      resolveACPMCPServers(req.Config),
		MCPExclusive:    req.Config.MCPExclusive,
	}
}

func cursorSessionPrecheck(req agentStartRequest) error {
	if len(req.Config.DisallowTools) > 0 {
		return capabilityRefusal(ProviderCursor, CapabilityToolRestrictions, cursorToolRestrictionsReason)
	}
	if mode := req.Config.PermissionMode; mode != "" && mode != "bypassPermissions" {
		return capabilityRefusal(ProviderCursor, CapabilityPermissionMode, cursorPermissionModeReason)
	}
	if len(req.Config.ExtraArgs) > 0 {
		return capabilityRefusal(ProviderCursor, CapabilityExtraArgs, cursorExtraArgsReason)
	}
	if req.Config.SandboxMode != "" {
		return capabilityRefusal(ProviderCursor, CapabilitySandboxPolicy, sandboxPolicyIsCodexOnlyReason)
	}
	return nil
}

func startCursorAgent(req agentStartRequest) (*agentStart, error) {
	if err := cursorSessionPrecheck(req); err != nil {
		return nil, err
	}

	bin, err := resolveCursorBin()
	if err != nil {
		return nil, err
	}

	plan := planCursorSession(req)
	// Stdio cannot be adopted after the coordinator dies. Reap leftover
	// writers (persisted PID + anyone holding store.db) before minting
	// so Launch cannot stack a second client (🎯T541.1).
	if req.Config.ConnectPID > 0 && req.Config.ConnectURL == "" {
		ReapCursorACPLeftovers(plan.PreferSessionID, req.Config.ConnectPID)
	} else if plan.PreferSessionID != "" {
		ReapCursorACPLeftovers(plan.PreferSessionID, 0)
	}
	var agentRef atomic.Pointer[Agent]
	onEvent := func(ev Event) {
		if a := agentRef.Load(); a != nil {
			a.publishEvent(ev)
		}
	}
	onClose := func() {
		if a := agentRef.Load(); a != nil {
			a.mu.Lock()
			a.alive = false
			a.mu.Unlock()
		}
	}

	client, err := startCursorACP(bin, plan.WorkDir, plan.Model, plan.PreferSessionID, plan.RequireResume, plan.MCPServers, nil, onEvent, onClose)
	if err != nil {
		if plan.PreferSessionID != "" {
			ReapCursorACPLeftovers(plan.PreferSessionID, 0)
		}
		return nil, err
	}

	sid := client.SessionID()
	pid := 0
	if client.cmd != nil && client.cmd.Process != nil {
		pid = client.cmd.Process.Pid
	}
	ops := agentOps{
		attachCommand: func(*Agent) string { return "" },
		interrupt: func(*Agent) error {
			return client.Cancel()
		},
		send: func(_ *Agent, msg string) error {
			return client.Prompt(msg)
		},
		stop: func(*Agent) {
			client.Close()
		},
		promptInFlight: func(*Agent) bool {
			return client.promptInFlight()
		},
	}
	return &agentStart{
		WindowID:   "cursor-acp-" + sid,
		Ops:        ops,
		TailJSONL:  false,
		SessionID:  sid,
		ConnectPID: pid,
		DetectReady: func(a *Agent) {
			agentRef.Store(a)
			select {
			case <-a.ready:
			default:
				close(a.ready)
			}
		},
	}, nil
}

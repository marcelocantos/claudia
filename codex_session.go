// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
)

// codexAppServerClient is a JSONL JSON-RPC client for `codex app-server`.
// Transport is parent-owned stdio. Notifications become [Event]s via
// parseCodexAppServerLine / agentEvent (🎯T4.4 / 🎯T4.5).
type codexAppServerClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan []byte
	closed  bool

	threadID string
	turnID   string
	model    string
	inFlight bool

	onEvent func(Event)
	onClose func()
}

func codexThreadStartParams(req agentStartRequest) codexAppServerThreadStartParams {
	ephemeral := false
	return codexAppServerThreadStartParams{
		CWD:            req.WorkDir,
		Model:          req.Config.Model,
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
		Ephemeral:      &ephemeral,
	}
}

func startCodexAppServer(bin, workDir, model, sessionID string, requireResume bool, onEvent func(Event), onClose func()) (*codexAppServerClient, error) {
	cmd := exec.Command(bin, "app-server")
	cmd.Dir = workDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("codex app-server stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("codex app-server stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}

	c := &codexAppServerClient{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		pending: make(map[int64]chan []byte),
		onEvent: onEvent,
		onClose: onClose,
	}
	go c.drainStderr()
	go c.readLoop()

	if err := c.initialize(); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openThread(workDir, model, sessionID, requireResume); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *codexAppServerClient) ThreadID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threadID
}

func (c *codexAppServerClient) Model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model
}

func (c *codexAppServerClient) promptInFlight() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight
}

func (c *codexAppServerClient) drainStderr() {
	if c.stderr == nil {
		return
	}
	sc := bufio.NewScanner(c.stderr)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		slog.Debug("codex app-server stderr", "line", sc.Text())
	}
}

func (c *codexAppServerClient) readLoop() {
	defer func() {
		c.mu.Lock()
		c.closed = true
		c.inFlight = false
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
	sc := bufio.NewScanner(c.stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		c.dispatch(line)
	}
}

func (c *codexAppServerClient) dispatch(line []byte) {
	var header struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		slog.Debug("codex app-server ignore non-json", "err", err)
		return
	}
	if header.ID != nil {
		c.mu.Lock()
		ch := c.pending[*header.ID]
		if ch != nil {
			delete(c.pending, *header.ID)
		}
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- line:
			default:
			}
		}
	}

	ev, ok, err := parseCodexAppServerLine(line)
	if err != nil || !ok {
		return
	}
	c.mu.Lock()
	if ev.ThreadID != "" {
		c.threadID = ev.ThreadID
	}
	if ev.TurnID != "" {
		c.turnID = ev.TurnID
	}
	if ev.Model != "" {
		c.model = ev.Model
	}
	if ev.Method == "turn/completed" {
		c.inFlight = false
	}
	onEvent := c.onEvent
	c.mu.Unlock()
	if onEvent == nil {
		return
	}
	if agentEv, ok := ev.agentEvent(); ok {
		onEvent(agentEv)
	}
}

func (c *codexAppServerClient) request(req codexAppServerRequest) ([]byte, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("codex app-server: client closed")
	}
	id := atomic.AddInt64(&c.nextID, 1)
	rid := int(id)
	req.ID = &rid
	ch := make(chan []byte, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	line, ok := <-ch
	if !ok {
		return nil, fmt.Errorf("codex app-server: closed waiting for %s", req.Method)
	}
	parsed, ok, err := parseCodexAppServerLine(line)
	if err != nil {
		return nil, err
	}
	if ok && parsed.IsError {
		return line, fmt.Errorf("codex app-server %s: %s", req.Method, parsed.ErrorMsg)
	}
	return line, nil
}

func (c *codexAppServerClient) write(req codexAppServerRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stdin == nil {
		return fmt.Errorf("codex app-server: client closed")
	}
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *codexAppServerClient) initialize() error {
	_, err := c.request(codexAppServerInitialize(0, codexAppServerClientInfo{
		Name:    "claudia",
		Title:   "claudia",
		Version: Version,
	}))
	if err != nil {
		return err
	}
	return c.write(codexAppServerInitialized())
}

func (c *codexAppServerClient) openThread(workDir, model, sessionID string, requireResume bool) error {
	tryResume := sessionID != "" && (requireResume || looksLikeCodexThreadID(sessionID))
	if tryResume {
		line, err := c.request(codexAppServerThreadResume(0, codexAppServerThreadIDParams{
			ThreadID:       sessionID,
			CWD:            workDir,
			Model:          model,
			ApprovalPolicy: "never",
			Sandbox:        "read-only",
		}))
		if err == nil {
			c.applyThreadResult(line, sessionID)
			return nil
		}
		if requireResume {
			return fmt.Errorf("session %s: existing Codex thread required but thread/resume failed: %w — refusing to mint a replacement session", sessionID, err)
		}
	}

	ephemeral := false
	line, err := c.request(codexAppServerThreadStart(0, codexAppServerThreadStartParams{
		CWD:            workDir,
		Model:          model,
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
		Ephemeral:      &ephemeral,
	}))
	if err != nil {
		return err
	}
	c.applyThreadResult(line, "")
	if c.ThreadID() == "" {
		return fmt.Errorf("codex app-server: thread/start returned no thread id")
	}
	return nil
}

func (c *codexAppServerClient) applyThreadResult(line []byte, fallbackID string) {
	ev, ok, err := parseCodexAppServerLine(line)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && ok {
		if ev.ThreadID != "" {
			c.threadID = ev.ThreadID
		} else if fallbackID != "" {
			c.threadID = fallbackID
		}
		if ev.Model != "" {
			c.model = ev.Model
		}
		return
	}
	if fallbackID != "" {
		c.threadID = fallbackID
	}
}

func looksLikeCodexThreadID(id string) bool {
	return len(id) >= 4 && id[:4] == "thr_"
}

func (c *codexAppServerClient) Prompt(text string) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("codex app-server: client closed")
	}
	if c.threadID == "" {
		c.mu.Unlock()
		return fmt.Errorf("codex app-server: no thread")
	}
	if c.inFlight {
		c.mu.Unlock()
		return fmt.Errorf("codex app-server: turn already in flight")
	}
	c.inFlight = true
	threadID := c.threadID
	c.mu.Unlock()

	_, err := c.request(codexAppServerTurnStart(0, codexAppServerTurnStartParams{
		ThreadID:       threadID,
		Input:          []codexAppServerUserInput{{Type: "text", Text: text}},
		ApprovalPolicy: "never",
	}))
	if err != nil {
		c.mu.Lock()
		c.inFlight = false
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *codexAppServerClient) Interrupt() error {
	c.mu.Lock()
	threadID := c.threadID
	turnID := c.turnID
	c.mu.Unlock()
	if threadID == "" || turnID == "" {
		return fmt.Errorf("codex app-server: no in-flight turn to interrupt")
	}
	_, err := c.request(codexAppServerTurnInterrupt(0, threadID, turnID))
	return err
}

func (c *codexAppServerClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	stdin := c.stdin
	c.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

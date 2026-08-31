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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Handshake RPCs (initialize / thread/start / thread/resume) must not
// block the caller's MCP tools/call forever. Cursor's client deadline is
// ~60s; a hang here wedges every later jevons_* call (jevons 🎯T545.1.1).
// Prompt/turn stays unbounded — those are the work, not the mint.
var codexAppServerHandshakeTimeout = 20 * time.Second

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

	threadID      string
	turnID        string
	lastTurnID    string
	model         string
	sandbox       string
	sandboxTuning codexSandboxTuning
	inFlight      bool
	exclusiveHome string
	workDir       string

	onEvent func(Event)
	onClose func()
}

const defaultCodexSandbox = "read-only"

func resolveCodexSandbox(mode string) string {
	if mode == "" {
		return defaultCodexSandbox
	}
	return mode
}

func codexThreadStartParams(req agentStartRequest) codexAppServerThreadStartParams {
	ephemeral := false
	return codexAppServerThreadStartParams{
		CWD:            req.WorkDir,
		Model:          req.Config.Model,
		ApprovalPolicy: "never",
		Sandbox:        resolveCodexSandbox(req.Config.SandboxMode),
		Ephemeral:      &ephemeral,
	}
}

func startCodexAppServer(bin, workDir, model, sessionID string, requireResume bool, sandbox string, tuning codexSandboxTuning, extraEnv []string, onEvent func(Event), onClose func()) (*codexAppServerClient, error) {
	cmd := exec.Command(bin, "app-server")
	cmd.Dir = workDir
	if len(extraEnv) > 0 {
		cmd.Env = appendEnv(nil, extraEnv)
	}
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
		cmd:           cmd,
		stdin:         stdin,
		stdout:        stdout,
		stderr:        stderr,
		sandbox:       resolveCodexSandbox(sandbox),
		sandboxTuning: tuning,
		pending:       make(map[int64]chan []byte),
		exclusiveHome: envValue(extraEnv, "CODEX_HOME"),
		workDir:       workDir,
		onEvent:       onEvent,
		onClose:       onClose,
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

// SetModel selects the model for subsequent turn/start calls (🎯T54).
func (c *codexAppServerClient) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("codex app-server: model must be non-empty")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("codex app-server: client closed")
	}
	if c.inFlight {
		return fmt.Errorf("codex app-server: turn already in flight")
	}
	c.model = model
	return nil
}

func (c *codexAppServerClient) sandboxMode() string {
	return resolveCodexSandbox(c.sandbox)
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
	sc := newACPLineScanner(c.stdout)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		c.dispatch(line)
	}
	logACPScanErr("codex", sc.Err())
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
		c.lastTurnID = ev.TurnID
	}
	if ev.Model != "" {
		c.model = ev.Model
	}
	if ev.ThreadID == "" {
		ev.ThreadID = c.threadID
	}
	if ev.TurnID == "" && c.inFlight {
		ev.TurnID = c.turnID
	}
	if ev.Method == "turn/completed" {
		c.inFlight = false
		c.turnID = ""
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
	line, err := c.waitPending(ch, id, req.Method, 0)
	if err != nil {
		return nil, err
	}
	return c.parseRPCResult(line, req.Method)
}

func (c *codexAppServerClient) requestHandshake(req codexAppServerRequest) ([]byte, error) {
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
	line, err := c.waitPending(ch, id, req.Method, codexAppServerHandshakeTimeout)
	if err != nil {
		return nil, err
	}
	return c.parseRPCResult(line, req.Method)
}

func (c *codexAppServerClient) waitPending(ch <-chan []byte, id int64, method string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		line, ok := <-ch
		if !ok {
			return nil, fmt.Errorf("codex app-server: closed waiting for %s", method)
		}
		return line, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("codex app-server: closed waiting for %s", method)
		}
		return line, nil
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("codex app-server: timeout waiting for %s after %s", method, timeout)
	}
}

func (c *codexAppServerClient) parseRPCResult(line []byte, method string) ([]byte, error) {
	parsed, ok, err := parseCodexAppServerLine(line)
	if err != nil {
		return nil, err
	}
	if ok && parsed.IsError {
		return line, fmt.Errorf("codex app-server %s: %s", method, parsed.ErrorMsg)
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
	_, err := c.requestHandshake(codexAppServerInitialize(0, codexAppServerClientInfo{
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
	// Any caller-supplied id is a resume candidate. Live Codex thread
	// ids are not thr_-prefixed (2026-08-17: 01a00f11-… ULIDs).
	tryResume := sessionID != ""
	if tryResume {
		line, err := c.requestHandshake(codexAppServerThreadResume(0, codexAppServerThreadIDParams{
			ThreadID:       sessionID,
			CWD:            workDir,
			Model:          model,
			ApprovalPolicy: "never",
			Sandbox:        c.sandboxMode(),
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
	line, err := c.requestHandshake(codexAppServerThreadStart(0, codexAppServerThreadStartParams{
		CWD:            workDir,
		Model:          model,
		ApprovalPolicy: "never",
		Sandbox:        c.sandboxMode(),
		Ephemeral:      &ephemeral,
	}))
	if err != nil {
		return err
	}
	c.applyThreadResult(line, "")
	if c.ThreadID() == "" {
		return fmt.Errorf("codex app-server: thread/start returned no thread id")
	}
	c.persistStartRollout()
	return nil
}

// persistStartRollout asks live Codex to write its own session_meta.
// Forged jsonl is found by thread/resume and then rejected ("rollout
// is empty"). thread/name/set is the persist that does not start a turn.
func (c *codexAppServerClient) persistStartRollout() {
	if c == nil {
		return
	}
	home := strings.TrimSpace(c.exclusiveHome)
	tid := c.ThreadID()
	if home == "" || tid == "" {
		return
	}
	if findCodexRollout(home, tid) != "" {
		return
	}
	if _, err := c.requestHandshake(codexAppServerThreadNameSet(0, tid, tid)); err != nil {
		slog.Warn("persist Codex start rollout", "home", home, "thread", tid, "err", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if findCodexRollout(home, tid) != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	slog.Warn("persist Codex start rollout: no file after thread/name/set", "home", home, "thread", tid)
}

// checkEffectiveSandbox compares the sandbox the app-server reports for a
// freshly started thread against what was asked for (🎯T598).
//
// This exists because asking is not getting. thread/start accepts an
// unknown sandboxPolicy and discards it in silence — a deliberately bogus
// value was accepted in testing — so "the call returned no error" says
// nothing about the sandbox a seat actually runs in. A seat that believes
// it can write its gate record and cannot will look healthy and fail only
// at its first gate, which is the failure this whole target came from.
//
// A mismatch is logged loudly rather than fatal: refusing to start would
// take the fleet down over a Codex schema change, and a seat that runs
// with a narrower sandbox can still do useful work — it just must not do
// so silently.
func checkEffectiveSandbox(want string, tuning codexSandboxTuning, effective codexEffectiveSandbox, session string) {
	if effective.Type == "" {
		return // nothing reported; no claim to check
	}
	wantType := codexSandboxTypeFor(want)
	if wantType != "" && effective.Type != wantType {
		slog.Error("codex sandbox is not what was requested",
			"session", session, "requested", want, "effective", effective.Type)
		return
	}
	if tuning.NetworkAccess && !effective.NetworkAccess {
		slog.Error("codex sandbox denied the requested network access",
			"session", session, "effective", effective.Type,
			"hint", "CODEX_HOME/config.toml [sandbox_workspace_write] network_access")
	}
	for _, want := range tuning.WritableRoots {
		if want == "" || slices.Contains(effective.WritableRoots, want) {
			continue
		}
		slog.Error("codex sandbox omitted a requested writable root",
			"session", session, "root", want, "effective", effective.WritableRoots)
	}
}

// codexSandboxTypeFor maps the wire mode name onto the type the
// app-server echoes back.
func codexSandboxTypeFor(mode string) string {
	switch mode {
	case "workspace-write":
		return "workspaceWrite"
	case "read-only":
		return "readOnly"
	case "danger-full-access":
		return "dangerFullAccess"
	}
	return ""
}

// codexEffectiveSandbox is the sandbox object thread/start echoes.
type codexEffectiveSandbox struct {
	Type          string   `json:"type"`
	NetworkAccess bool     `json:"networkAccess"`
	WritableRoots []string `json:"writableRoots"`
}

func parseEffectiveSandbox(line []byte) codexEffectiveSandbox {
	var envelope struct {
		Result struct {
			Thread struct {
				Sandbox codexEffectiveSandbox `json:"sandbox"`
			} `json:"thread"`
		} `json:"result"`
	}
	_ = json.Unmarshal(line, &envelope)
	return envelope.Result.Thread.Sandbox
}

func (c *codexAppServerClient) applyThreadResult(line []byte, fallbackID string) {
	checkEffectiveSandbox(c.sandboxMode(), c.sandboxTuning, parseEffectiveSandbox(line), c.threadID)
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
	c.turnID = ""
	threadID := c.threadID
	model := c.model
	c.mu.Unlock()

	params := codexAppServerTurnStartParams{
		ThreadID:       threadID,
		Input:          []codexAppServerUserInput{{Type: "text", Text: text}},
		ApprovalPolicy: "never",
	}
	if model != "" {
		params.Model = model
	}
	_, err := c.request(codexAppServerTurnStart(0, params))
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
	if turnID == "" {
		turnID = c.lastTurnID
	}
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
	if c.cmd != nil && c.cmd.Process != nil {
		// SIGTERM first so Codex can flush sqlite/jsonl. Closing stdin
		// then SIGKILL was the bounce hole: thread/resume → no rollout
		// (jevons 🎯T545.1.2).
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	gracefulProcessExit(c.cmd, 3*time.Second)
}

func gracefulProcessExit(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if grace <= 0 {
		grace = time.Second
	}
	select {
	case <-done:
		return
	case <-time.After(grace):
		_ = cmd.Process.Kill()
		<-done
	}
}

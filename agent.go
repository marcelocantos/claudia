// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package claudia embeds Claude Code agents in Go programs.
//
// It provides two modes of operation:
//
//   - Task mode: [Run] sends a single prompt to Claude Code, waits for
//     completion, and returns the result text. Suitable for one-shot
//     code generation, analysis, or transformation tasks.
//
//   - Session mode: [Start] spawns a persistent Claude Code process.
//     Use [Agent.Send] to send messages, [Agent.OnEvent] to observe
//     JSONL events, and [Agent.WaitForResponse] to block until the
//     next assistant turn completes.
//
// Both modes manage the underlying PTY and JSONL transcript tailing
// automatically. Claude Code's instability is absorbed behind a clean API.
package claudia

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
)

// Config configures a Claude Code agent.
type Config struct {
	// WorkDir is the working directory for the Claude Code process.
	// Defaults to ".".
	WorkDir string

	// SessionID is a persistent session ID. If empty, a new random
	// session is created. If non-empty and the session JSONL already
	// exists, the session is resumed with --resume.
	SessionID string

	// Model overrides the default Claude model (e.g. "opus", "sonnet").
	Model string

	// PermissionMode sets the Claude Code permission mode.
	// Defaults to "bypassPermissions".
	PermissionMode string

	// MCPConfig is the path to an MCP config JSON file.
	// Empty means Claude Code uses its default discovery.
	MCPConfig string

	// DisallowTools is a comma-separated list of additional tools to
	// disallow. Agent, TeamCreate, TeamDelete, SendMessage, and
	// EnterWorktree are always disallowed.
	DisallowTools string

	// ExtraArgs are additional CLI arguments passed to claude.
	ExtraArgs []string

	// TermLogPath is the path to which raw PTY output (including ANSI
	// escapes) is appended. If empty, it defaults to
	// $XDG_STATE_HOME/claudia/terms/<escaped-workdir>/<sessionID>.term
	// (with $XDG_STATE_HOME defaulting to ~/.local/state). Set to "-"
	// to disable terminal logging.
	TermLogPath string
}

// Agent is a persistent Claude Code process running in a PTY.
type Agent struct {
	sessionID   string
	jsonlPath   string
	termLogPath string
	ptmx        *os.File
	cmd         *exec.Cmd

	// daemonClient is the opportunistic claudiad connection set up
	// during Start. nil when no daemon is running; that's the normal
	// fallback path and not an error condition. Used only to report
	// session lifecycle to the daemon; the library otherwise operates
	// independently of it.
	daemonClient *daemonClient

	mu       sync.Mutex
	alive    bool
	onEvent  EventFunc
	stopOnce sync.Once

	// Terminal output streaming. termMu also guards termLog writes,
	// termLog close, and lastTermWrite, so Stop cannot close the
	// file while pushTermOutput is mid-write.
	termMu        sync.Mutex
	termBuf       []byte
	termSubs      []chan []byte
	termLog       *os.File
	lastTermWrite time.Time

	// TUI readiness. ready closes once detectReady concludes, either
	// because the TUI has quiesced (success, readyErr == nil) or
	// because detection gave up (failure, readyErr set). Send blocks
	// on this channel before writing to the PTY.
	ready    chan struct{}
	readyErr error
}

// Readiness detection tuning. These are not currently exposed via
// Config — the defaults match the startup profile observed in
// cmd/probe-ready on a standalone session (first PTY burst ~200ms,
// quiet by ~750ms). Expose them through Config if consumers need
// per-site tuning.
const (
	readyQuiescenceDuration = 500 * time.Millisecond
	readyOverallTimeout     = 30 * time.Second
	readyPollInterval       = 50 * time.Millisecond
)

// Start spawns a new Claude Code agent in a PTY.
func Start(cfg Config) (*Agent, error) {
	if cfg.WorkDir == "" {
		cfg.WorkDir = "."
	}
	workDir, _ := filepath.Abs(cfg.WorkDir)
	// Resolve symlinks so our project-dir escaping matches Claude
	// Code's own canonicalisation. On macOS, /var is a symlink to
	// /private/var, and any workdir under /var/folders (including
	// Go's t.TempDir()) produces a JSONL transcript under
	// -private-var-folders-..., while our unresolved path escapes
	// to -var-folders-... — we'd tail a file Claude never writes.
	// If resolution fails (path missing, permission denied) we fall
	// back to the unresolved Abs path rather than failing Start.
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}

	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "bypassPermissions"
	}

	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	jsonlDir := projectDir(workDir)
	jsonlPath := filepath.Join(jsonlDir, sessionID+".jsonl")

	termLogPath := cfg.TermLogPath
	switch termLogPath {
	case "":
		termLogPath = filepath.Join(termLogDir(workDir), sessionID+".term")
	case "-":
		termLogPath = ""
	}

	// If the JSONL already exists, this is a resume.
	_, statErr := os.Stat(jsonlPath)
	resuming := statErr == nil

	// Agents spawned by claudia are forbidden from creating their own
	// sub-agents. The host program owns the process lifecycle.
	disallowed := "Agent,TeamCreate,TeamDelete,SendMessage,EnterWorktree"
	if cfg.DisallowTools != "" {
		disallowed += "," + cfg.DisallowTools
	}

	args := []string{
		"--permission-mode", cfg.PermissionMode,
		"--disallowedTools", disallowed,
	}
	if resuming {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if cfg.MCPConfig != "" {
		args = append(args, "--mcp-config", cfg.MCPConfig)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, cfg.ExtraArgs...)

	cmd := exec.Command("claude", args...)
	cmd.Dir = workDir
	// Strip CLAUDECODE so the child doesn't detect itself as nested
	// when the host Go program is itself running under a Claude Code
	// harness. Task mode already does this; Session mode must match
	// or the child misbehaves (notably: JSONL transcript is never
	// written, which breaks event-driven consumers).
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")

	ptmx, pts, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("pty.Open: %w", err)
	}

	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		ptmx.Close()
		pts.Close()
		return nil, fmt.Errorf("start claude: %w", err)
	}
	pts.Close()

	a := &Agent{
		sessionID:   sessionID,
		jsonlPath:   jsonlPath,
		termLogPath: termLogPath,
		ptmx:        ptmx,
		cmd:         cmd,
		alive:       true,
		ready:       make(chan struct{}),
	}

	if termLogPath != "" {
		if err := os.MkdirAll(filepath.Dir(termLogPath), 0o755); err != nil {
			slog.Warn("term log mkdir failed", "path", termLogPath, "err", err)
		} else if f, err := os.OpenFile(termLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			slog.Warn("term log open failed", "path", termLogPath, "err", err)
		} else {
			a.termLog = f
		}
	}

	// Capture PTY output.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				a.pushTermOutput(data)
			}
			if err != nil {
				slog.Debug("pty read done", "err", err)
				return
			}
		}
	}()

	// Monitor process exit.
	go func() {
		err := cmd.Wait()
		slog.Debug("claude process exited", "session", sessionID, "err", err)
		a.mu.Lock()
		a.alive = false
		a.mu.Unlock()
	}()

	// Tail JSONL.
	go a.tailJSONL()

	// Detect TUI readiness so Send can safely block on it.
	go a.detectReady()

	// Opportunistically dial the claudiad daemon and report this
	// session. Best-effort: a missing daemon is the normal fallback
	// path, not a failure. The dial budget is intentionally short so
	// Start stays fast on hosts without the daemon.
	go a.maybeRegisterWithDaemon(workDir, cmd.Process.Pid)

	return a, nil
}

// maybeRegisterWithDaemon tries to dial claudiad and report this
// agent's (cwd, session_id, pid) tuple. Runs in a goroutine so a
// slow/unreachable daemon never blocks Start. On success, the
// client is cached on the Agent so Stop can close it cleanly.
func (a *Agent) maybeRegisterWithDaemon(cwd string, pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonDialBudget)
	defer cancel()

	client, err := dialDaemon(ctx)
	if err != nil {
		slog.Debug("daemon dial failed (fallback to direct spawn)", "err", err)
		return
	}
	if err := client.registerSession(ctx, cwd, a.sessionID, pid); err != nil {
		slog.Debug("daemon register failed", "err", err)
		client.Close()
		return
	}

	a.mu.Lock()
	a.daemonClient = client
	a.mu.Unlock()
}

// detectReady watches the PTY byte stream for quiescence — the
// point at which Claude Code's TUI has finished painting its
// startup UI and stops writing to the terminal until user input
// arrives. That silence is the readiness signal Send gates on.
//
// cmd/probe-ready confirms that a standalone `claude` session
// emits a burst of output (banner, config load, input prompt) in
// the first ~750ms of startup and then goes silent. A 500ms
// silence window is long enough to clear the initial burst and
// short enough that consumers don't notice the block.
//
// A prior version of this function also waited for the session's
// JSONL file to exist, on the theory that file existence meant
// "transcript initialised". That check was dead weight — the
// JSONL is written lazily, sometimes only after the first turn,
// and its appearance is not coupled to TUI readiness. Removing it
// fixed a 30s hang on every Send for sessions where the JSONL
// never materialised in time.
//
// On success, detectReady returns with readyErr == nil and the
// deferred close fires. On failure (process death, overall
// timeout) readyErr is set before the close.
func (a *Agent) detectReady() {
	defer close(a.ready)

	start := time.Now()
	for {
		a.termMu.Lock()
		hasData := len(a.termBuf) > 0
		last := a.lastTermWrite
		a.termMu.Unlock()

		if hasData && !last.IsZero() && time.Since(last) >= readyQuiescenceDuration {
			return
		}
		if !a.Alive() {
			a.readyErr = fmt.Errorf("claude exited before TUI became ready")
			return
		}
		if time.Since(start) > readyOverallTimeout {
			a.readyErr = fmt.Errorf("timeout waiting for TUI to quiesce after %s", readyOverallTimeout)
			return
		}
		time.Sleep(readyPollInterval)
	}
}

// WaitReady blocks until the TUI has finished initialising and is
// ready to accept input from Send, or until ctx is cancelled. It
// returns any error recorded during readiness detection (e.g. Claude
// exited during startup, or the overall timeout elapsed).
//
// Calling this is optional: Send calls it internally on every
// invocation, so consumers that just want to send a prompt do not
// need to wait explicitly. WaitReady is exposed for consumers that
// want to observe the ready transition (e.g. to update a UI) or
// distinguish "readiness failed" from "send failed".
func (a *Agent) WaitReady(ctx context.Context) error {
	select {
	case <-a.ready:
		return a.readyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run sends a single prompt to a new Claude Code session, waits for
// completion, and returns the assistant's response text.
func Run(ctx context.Context, prompt string, cfg Config) (string, error) {
	agent, err := Start(cfg)
	if err != nil {
		return "", err
	}
	defer agent.Stop()

	if err := agent.Send(prompt); err != nil {
		return "", fmt.Errorf("send prompt: %w", err)
	}

	return agent.WaitForResponse(ctx)
}

// SessionID returns the Claude Code session ID.
func (a *Agent) SessionID() string { return a.sessionID }

// JSONLPath returns the path to the session JSONL file.
func (a *Agent) JSONLPath() string { return a.jsonlPath }

// TermLogPath returns the path to the raw terminal output log, or ""
// if terminal logging is disabled.
func (a *Agent) TermLogPath() string { return a.termLogPath }

// Alive reports whether the Claude process is still running.
func (a *Agent) Alive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.alive
}

// OnEvent sets the callback for JSONL events. Only one callback is
// active at a time; setting a new one replaces the previous.
func (a *Agent) OnEvent(fn EventFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onEvent = fn
}

// Interrupt sends Esc to the Claude process to cancel the current turn.
func (a *Agent) Interrupt() error {
	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("claude process not running")
	}
	_, err := a.ptmx.Write([]byte("\x1b"))
	return err
}

// Send writes a user message to the Claude process and submits it.
//
// Send blocks until Claude Code's TUI has finished initialising and
// is ready to accept input (see [Agent.WaitReady] for the detection
// strategy). This prevents keystrokes from being typed into a
// half-painted startup UI where they would be silently dropped.
// Once the session has reached readiness, subsequent Send calls
// return immediately — the ready channel stays closed for the life
// of the agent.
//
// The message is submitted by appending a carriage return ("\r",
// 0x0D) — Claude Code's TUI treats this as the Enter key, which
// submits the current turn. A line feed ("\n", 0x0A) is treated as
// Shift+Enter instead: it inserts a newline into the input buffer
// without submitting. Early claudia releases used "\n" and silently
// accumulated prompts in the input area where they were never sent
// to the API. If you need to include a literal newline inside a
// single prompt, embed "\n" characters in msg — Send only appends
// the submit key.
//
// If readiness detection failed (process exited during startup, or
// the overall timeout elapsed) Send returns the detection error
// without writing anything.
func (a *Agent) Send(msg string) error {
	<-a.ready
	if a.readyErr != nil {
		return fmt.Errorf("claude not ready: %w", a.readyErr)
	}

	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("claude process not running")
	}
	data := []byte(msg + "\r")
	_, err := a.ptmx.Write(data)
	return err
}

// WaitForResponse blocks until the next assistant turn completes and
// returns the assistant text accumulated across the turn.
//
// A single logical assistant message can be split across multiple
// JSONL events — one per content block (thinking, text, tool_use,
// etc.). In some Claude Code versions every block in a message
// carries the message's stop_reason, not just the last one; in
// others only the final block does. WaitForResponse therefore does
// not resolve on the first terminal stop_reason it sees. Instead,
// it starts a short settle timer when a terminal event arrives and
// resets the timer on every subsequent assistant event. The
// accumulated text is returned only once the timer expires without
// new events — the heuristic for "all content blocks of this turn
// have arrived". The settle delay trades a small constant latency
// (waitSettleDuration) against the risk of emitting an incomplete
// message.
//
// Completion stop reasons are end_turn, stop_sequence, and
// max_tokens. A tool_use stop reason is not terminal: the model
// paused for tool results and will emit further assistant events
// as the turn continues, and those events will keep the settle
// timer from firing.
func (a *Agent) WaitForResponse(ctx context.Context) (string, error) {
	ch := make(chan string, 1)
	var (
		mu           sync.Mutex
		text         strings.Builder
		seenTerminal bool
		settleTimer  *time.Timer
	)

	emit := func() {
		mu.Lock()
		result := text.String()
		mu.Unlock()
		select {
		case ch <- result:
		default:
		}
	}

	oldFn := a.onEvent
	a.OnEvent(func(ev Event) {
		if oldFn != nil {
			oldFn(ev)
		}
		if ev.Type != "assistant" {
			return
		}

		mu.Lock()
		if ev.Text != "" {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(ev.Text)
		}
		if ev.IsTerminalStop() {
			seenTerminal = true
		}
		armed := seenTerminal
		if armed {
			if settleTimer != nil {
				settleTimer.Stop()
			}
			settleTimer = time.AfterFunc(waitSettleDuration, emit)
		}
		mu.Unlock()
	})

	defer a.OnEvent(oldFn)
	defer func() {
		mu.Lock()
		if settleTimer != nil {
			settleTimer.Stop()
		}
		mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-ch:
		return result, nil
	}
}

// waitSettleDuration is how long WaitForResponse lingers after
// seeing a terminal stop_reason to let any remaining content blocks
// of the same message arrive. 250ms is comfortably longer than the
// ~45ms gap observed between thinking and text blocks of a single
// message in Claude Code v2.1.101, while still short enough to be
// imperceptible to most consumers.
const waitSettleDuration = 250 * time.Millisecond

// Resize changes the PTY window size.
func (a *Agent) Resize(cols, rows uint16) error {
	return pty.Setsize(a.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// Stop terminates the Claude process.
func (a *Agent) Stop() {
	a.stopOnce.Do(func() {
		syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
		time.Sleep(time.Second)
		a.cmd.Process.Kill()
		a.ptmx.Close()

		a.termMu.Lock()
		if a.termLog != nil {
			a.termLog.Close()
			a.termLog = nil
		}
		a.termMu.Unlock()

		a.mu.Lock()
		client := a.daemonClient
		a.daemonClient = nil
		a.mu.Unlock()
		if client != nil {
			client.Close()
		}
	})
}

const termBufSize = 128 * 1024 // 128KB ring buffer

func (a *Agent) pushTermOutput(data []byte) {
	a.termMu.Lock()
	defer a.termMu.Unlock()

	a.lastTermWrite = time.Now()

	a.termBuf = append(a.termBuf, data...)
	if len(a.termBuf) > termBufSize {
		a.termBuf = a.termBuf[len(a.termBuf)-termBufSize:]
	}

	if a.termLog != nil {
		if _, err := a.termLog.Write(data); err != nil {
			slog.Warn("term log write failed", "path", a.termLogPath, "err", err)
			a.termLog.Close()
			a.termLog = nil
		}
	}

	for _, ch := range a.termSubs {
		select {
		case ch <- data:
		default:
		}
	}
}

// SubscribeTerminal returns a channel that receives live PTY output
// and the buffered recent output. Call [Agent.UnsubscribeTerminal] when done.
func (a *Agent) SubscribeTerminal() (history []byte, ch chan []byte) {
	a.termMu.Lock()
	defer a.termMu.Unlock()

	ch = make(chan []byte, 256)
	a.termSubs = append(a.termSubs, ch)

	history = make([]byte, len(a.termBuf))
	copy(history, a.termBuf)
	return
}

// UnsubscribeTerminal removes a terminal subscriber.
func (a *Agent) UnsubscribeTerminal(ch chan []byte) {
	a.termMu.Lock()
	defer a.termMu.Unlock()

	for i, c := range a.termSubs {
		if c == ch {
			a.termSubs = append(a.termSubs[:i], a.termSubs[i+1:]...)
			close(ch)
			return
		}
	}
}

func (a *Agent) tailJSONL() {
	// Wait for file to be created.
	for {
		if _, err := os.Stat(a.jsonlPath); err == nil {
			break
		}
		if !a.Alive() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	f, err := os.Open(a.jsonlPath)
	if err != nil {
		slog.Error("open JSONL failed", "session", a.sessionID, "err", err)
		return
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if !a.Alive() {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		ev := parseEvent(line)

		a.mu.Lock()
		fn := a.onEvent
		a.mu.Unlock()

		if fn != nil {
			fn(ev)
		}
	}
}

// projectDir returns the Claude Code project directory for a workdir.
func projectDir(workDir string) string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "projects", escapeWorkDir(workDir))
}

// termLogDir returns the directory under which raw terminal output
// logs are written for a given workdir. Follows XDG_STATE_HOME, with
// a ~/.local/state fallback.
func termLogDir(workDir string) string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(stateHome, "claudia", "terms", escapeWorkDir(workDir))
}

// escapeWorkDir applies Claude Code's workdir-escape scheme:
// non-alphanumeric/dash runes become '-'.
func escapeWorkDir(workDir string) string {
	var b strings.Builder
	for _, r := range workDir {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

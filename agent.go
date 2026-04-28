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
//   - Session mode: [Start] spawns a persistent Claude Code process
//     inside a tmux window on a dedicated claudia tmux server. Use
//     [Agent.Send] to send messages, [Agent.OnEvent] to observe JSONL
//     events, and [Agent.WaitForResponse] to block until the next
//     assistant turn completes.
//
// The tmux substrate provides crash-survival (agents survive consumer
// process death), human-attachable observability (tmux attach), and a
// foundation for the warm agent pool (see Acquire/Release in T1.2).
// JSONL transcript tailing drives the event stream regardless of
// transport.
package claudia

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
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

	// DisallowTools lists additional tool names to disallow. Agent,
	// TeamCreate, TeamDelete, SendMessage, and EnterWorktree are
	// always disallowed in addition to whatever appears here.
	DisallowTools []string

	// ExtraArgs are additional CLI arguments passed to claude.
	ExtraArgs []string

	// TermLogPath is the path to which raw terminal output (including
	// ANSI escapes) is appended. If empty, it defaults to
	// $XDG_STATE_HOME/claudia/terms/<escaped-workdir>/<sessionID>.term
	// (with $XDG_STATE_HOME defaulting to ~/.local/state). Set to "-"
	// to disable terminal logging.
	TermLogPath string

	// PoolPolicy controls what Acquire does when all matching pool
	// windows are held by other consumers.
	//   "spawn" (default): create a new window.
	//   "wait":            block until a window is released (currently
	//                      falls back to "spawn").
	//   "error":           return an error immediately.
	PoolPolicy string

	// PoolCap is the maximum number of pool windows (idle + held) for
	// this pool key. When Acquire would exceed the cap, the oldest idle
	// window is evicted. 0 means unlimited.
	PoolCap int
}

// Agent is a persistent Claude Code process running inside a tmux
// window on the dedicated claudia tmux server. The tmux substrate
// provides crash-survival (the agent stays alive if the consumer
// process dies) and human-attachable observability (see
// [Agent.AttachCommand]).
type Agent struct {
	sessionID    string
	jsonlPath    string
	termLogPath  string
	tmuxWindowID string
	tmuxCtrl     *tmuxagent.Control

	mu       sync.Mutex
	alive    bool
	onEvent  EventFunc
	stopOnce sync.Once

	// poolWindow is true when the agent was acquired from the warm pool
	// (via Acquire) rather than spawned fresh (via Start). Pool agents
	// must be released via Release rather than stopped with Stop.
	poolWindow bool
	// poolWorkDir is the resolved absolute working directory used as
	// part of the pool key. Set by buildPoolAgent; empty for Start agents.
	poolWorkDir string

	// Terminal output streaming. termMu also guards termLog writes
	// and termLog close, so Stop cannot close the file while
	// pushTermOutput is mid-write.
	termMu   sync.Mutex
	termBuf  []byte
	termSubs []chan []byte
	termLog  *os.File

	// TUI readiness. ready closes once detectReady concludes, either
	// because the capture-pane regex matched (success, readyErr == nil)
	// or because detection gave up (failure, readyErr set). Send
	// blocks on this channel before writing to the agent.
	ready    chan struct{}
	readyErr error
}

// Readiness detection tuning.
const (
	readyOverallTimeout = 30 * time.Second
	readyPollInterval   = 50 * time.Millisecond
)

// checkTmux returns a clear error if tmux is not on PATH.
func checkTmux() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux is required for claudia Session mode but was not found on PATH; install it via: brew install tmux (macOS) or apt install tmux (Linux)")
	}
	return nil
}

// Start spawns a new Claude Code agent inside a tmux window on the
// dedicated claudia tmux server.
func Start(cfg Config) (*Agent, error) {
	if err := checkTmux(); err != nil {
		return nil, err
	}
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
	jsonlPath := SessionJSONLPath(sessionID, workDir)

	termLogPath := cfg.TermLogPath
	switch termLogPath {
	case "":
		termLogPath = filepath.Join(termLogDir(workDir), sessionID+".term")
	case "-":
		termLogPath = ""
	}

	// If the JSONL already exists, this is a resume.
	resuming, err := SessionExists(sessionID, workDir)
	if err != nil {
		return nil, fmt.Errorf("check session JSONL: %w", err)
	}

	// Agents spawned by claudia are forbidden from creating their own
	// sub-agents. The host program owns the process lifecycle.
	disallowed := "Agent,TeamCreate,TeamDelete,SendMessage,EnterWorktree"
	if len(cfg.DisallowTools) > 0 {
		disallowed += "," + strings.Join(cfg.DisallowTools, ",")
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

	a := &Agent{
		sessionID:   sessionID,
		jsonlPath:   jsonlPath,
		termLogPath: termLogPath,
		alive:       true,
		ready:       make(chan struct{}),
	}

	// Open terminal log.
	if termLogPath != "" {
		if err := os.MkdirAll(filepath.Dir(termLogPath), 0o755); err != nil {
			slog.Warn("term log mkdir failed", "path", termLogPath, "err", err)
		} else if f, err := os.OpenFile(termLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			slog.Warn("term log open failed", "path", termLogPath, "err", err)
		} else {
			a.termLog = f
		}
	}

	// Spawn the claude window on the dedicated tmux server.
	windowName := "claudia-" + sessionID[:8]
	claudeBin, err := resolveClaudeBin()
	if err != nil {
		return nil, err
	}
	windowID, err := tmuxagent.SpawnWindow(workDir, windowName, claudeBin, args)
	if err != nil {
		return nil, fmt.Errorf("tmux spawn: %w", err)
	}
	a.tmuxWindowID = windowID

	// Store session ID on the window for crash-survival recovery.
	if err := tmuxagent.SetWindowOption(windowID, "claudia-session-id", sessionID); err != nil {
		slog.Warn("failed to set session ID on tmux window", "err", err)
	}

	slog.Info("claudia agent started",
		"session", sessionID,
		"window", windowID,
		"attach", a.AttachCommand())

	// Register this session as the start of a new chain. The chain ID
	// equals the session ID for freshly-started agents. /clear detection
	// (which links subsequent sessions to the same chain) is deferred to
	// a follow-up target.
	if err := RegisterChain(sessionID, sessionID); err != nil {
		slog.Warn("failed to register session chain", "session", sessionID, "err", err)
	}

	// Dial control mode for terminal byte stream.
	ctrl, err := tmuxagent.DialControl(windowID)
	if err != nil {
		tmuxagent.KillWindow(windowID)
		return nil, fmt.Errorf("tmux control-mode: %w", err)
	}
	a.tmuxCtrl = ctrl

	// Route control-mode bytes → pushTermOutput.
	go func() {
		for data := range ctrl.Bytes() {
			a.pushTermOutput(data)
		}
		slog.Debug("tmux control stream closed", "session", sessionID)
		a.mu.Lock()
		a.alive = false
		a.mu.Unlock()
	}()

	// Tail JSONL — events come from the transcript file, not from
	// terminal bytes.
	go a.tailJSONL()

	// Detect TUI readiness so Send can safely block on it.
	go a.detectReady()

	return a, nil
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

// SessionJSONLPath returns the path Claude Code would use for the
// given session ID and workdir, whether or not the file exists. The
// path is ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl, where
// the cwd encoding maps non-alphanumeric/dash runes to '-'. Callers
// embedding claudia (e.g. building a "resume meeting" flow) can use
// this together with [SessionExists] to decide between fresh-start
// and resume code paths before invoking [Start].
func SessionJSONLPath(sessionID, workDir string) string {
	return filepath.Join(projectDir(workDir), sessionID+".jsonl")
}

// SessionExists reports whether a Claude Code JSONL transcript exists
// on disk for the given session ID and workdir. It returns (false,
// nil) when the file is simply absent — only filesystem errors
// (permission denied, etc.) are propagated.
//
// Embedders should prefer this over reproducing the path computation
// themselves; the encoded-cwd convention is owned by Claude Code and
// claudia tracks it here.
func SessionExists(sessionID, workDir string) (bool, error) {
	_, err := os.Stat(SessionJSONLPath(sessionID, workDir))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// TermLogPath returns the path to the raw terminal output log, or ""
// if terminal logging is disabled.
func (a *Agent) TermLogPath() string { return a.termLogPath }

// AttachCommand returns the tmux incantation a human can paste to
// watch the live agent session. This is the primary observability
// mechanism for claudia agents.
func (a *Agent) AttachCommand() string {
	return fmt.Sprintf("tmux -S %s attach -t %s", tmuxagent.SocketPath(), a.tmuxWindowID)
}

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

// Interrupt sends the Escape key to the Claude process to cancel
// the current turn.
func (a *Agent) Interrupt() error {
	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("claude process not running")
	}
	return tmuxagent.SendEscape(a.tmuxWindowID)
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
// The message is typed literally via tmux send-keys -l, then
// submitted with CR (Enter). Embedded newlines in msg become
// Shift+Enter (insert newline without submitting) — matching Claude
// Code's TUI semantics.
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
	return tmuxagent.SendKeys(a.tmuxWindowID, msg)
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

// Resize changes the terminal dimensions.
func (a *Agent) Resize(cols, rows uint16) error {
	return tmuxagent.ResizeWindow(a.tmuxWindowID, cols, rows)
}

// Stop terminates the Claude process and releases resources.
func (a *Agent) Stop() {
	a.stopOnce.Do(func() {
		tmuxagent.KillWindow(a.tmuxWindowID)
		if a.tmuxCtrl != nil {
			a.tmuxCtrl.Close()
		}

		a.termMu.Lock()
		if a.termLog != nil {
			a.termLog.Close()
			a.termLog = nil
		}
		a.termMu.Unlock()
	})
}

// detectReady polls capture-pane for Claude Code's idle input box
// at the bottom of the rendered viewport. The empty-prompt-box regex
// matches a horizontal rule, the ❯ prompt glyph, another horizontal
// rule, and up to 5 trailing status lines.
//
// This replaces the earlier PTY-silence heuristic. The rendered-frame
// approach is cleaner: it detects a fixed-point visual state rather
// than inferring readiness from the absence of byte traffic.
func (a *Agent) detectReady() {
	defer close(a.ready)

	_, err := tmuxagent.WaitReady(a.tmuxWindowID, readyPollInterval, readyOverallTimeout)
	if err != nil {
		a.readyErr = err
	}
}

const termBufSize = 128 * 1024 // 128KB ring buffer

func (a *Agent) pushTermOutput(data []byte) {
	a.termMu.Lock()
	defer a.termMu.Unlock()

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

// SubscribeTerminal returns a channel that receives live terminal
// output and the buffered recent output. Call
// [Agent.UnsubscribeTerminal] when done.
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

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
}

// Agent is a persistent Claude Code process running in a PTY.
type Agent struct {
	sessionID string
	jsonlPath string
	ptmx      *os.File
	cmd       *exec.Cmd

	mu       sync.Mutex
	alive    bool
	onEvent  EventFunc
	stopOnce sync.Once

	// Terminal output streaming.
	termMu   sync.Mutex
	termBuf  []byte
	termSubs []chan []byte
}

// Start spawns a new Claude Code agent in a PTY.
func Start(cfg Config) (*Agent, error) {
	if cfg.WorkDir == "" {
		cfg.WorkDir = "."
	}
	workDir, _ := filepath.Abs(cfg.WorkDir)

	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "bypassPermissions"
	}

	sessionID := cfg.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	jsonlDir := projectDir(workDir)
	jsonlPath := filepath.Join(jsonlDir, sessionID+".jsonl")

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
		sessionID: sessionID,
		jsonlPath: jsonlPath,
		ptmx:      ptmx,
		cmd:       cmd,
		alive:     true,
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

// Send writes a user message to the Claude process.
func (a *Agent) Send(msg string) error {
	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("claude process not running")
	}
	data := []byte(msg + "\n")
	_, err := a.ptmx.Write(data)
	return err
}

// WaitForResponse blocks until the next assistant response is complete
// (signalled by a "system" event in the JSONL). Returns the assistant text.
func (a *Agent) WaitForResponse(ctx context.Context) (string, error) {
	ch := make(chan string, 1)
	var text strings.Builder

	oldFn := a.onEvent
	a.OnEvent(func(ev Event) {
		if oldFn != nil {
			oldFn(ev)
		}
		switch ev.Type {
		case "assistant":
			if ev.Text != "" {
				text.WriteString(ev.Text)
			}
		case "system":
			select {
			case ch <- text.String():
			default:
			}
		}
	})

	defer a.OnEvent(oldFn)

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-ch:
		return result, nil
	}
}

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
	})
}

const termBufSize = 128 * 1024 // 128KB ring buffer

func (a *Agent) pushTermOutput(data []byte) {
	a.termMu.Lock()
	defer a.termMu.Unlock()

	a.termBuf = append(a.termBuf, data...)
	if len(a.termBuf) > termBufSize {
		a.termBuf = a.termBuf[len(a.termBuf)-termBufSize:]
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
	var escaped strings.Builder
	for _, r := range workDir {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			escaped.WriteRune(r)
		} else {
			escaped.WriteByte('-')
		}
	}
	return filepath.Join(os.Getenv("HOME"), ".claude", "projects", escaped.String())
}

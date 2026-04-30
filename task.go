// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// TaskEventType identifies the kind of task event.
type TaskEventType string

const (
	// TaskEventInit is emitted once at the start of a run. It carries
	// the Claude session ID in TaskEvent.SessionID.
	TaskEventInit TaskEventType = "init"

	// TaskEventText carries a text block from the assistant's response
	// in TaskEvent.Content.
	TaskEventText TaskEventType = "text"

	// TaskEventToolUse is emitted when Claude invokes a tool. The tool
	// name, ID, and JSON-encoded input are in TaskEvent.ToolName,
	// ToolID, and ToolInput respectively.
	TaskEventToolUse TaskEventType = "tool_use"

	// TaskEventResult is the final event in a successful run. It
	// contains the assistant's full result text and usage/cost data.
	TaskEventResult TaskEventType = "result"

	// TaskEventError is emitted when Claude Code reports a run-level
	// error. TaskEvent.IsError is true and TaskEvent.ErrorMsg contains
	// the combined error message(s).
	TaskEventError TaskEventType = "error"
)

// Usage holds token counts from a result event.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	// CacheCreationInputTokens is tokens written to the prompt cache.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// CacheReadInputTokens is tokens read from the prompt cache (billed
	// at a lower rate than uncached input tokens).
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// TaskEvent is a parsed event from Claude Code's stream-json output.
type TaskEvent struct {
	// Type identifies the event kind; see the TaskEvent* constants.
	Type TaskEventType

	// Content is the text content (TaskEventText) or final result text
	// (TaskEventResult).
	Content string

	// ToolName is the tool being invoked (TaskEventToolUse only).
	ToolName string

	// ToolInput is the JSON-encoded tool input object (TaskEventToolUse only).
	ToolInput string

	// ToolID is the unique tool-call identifier (TaskEventToolUse only).
	ToolID string

	// SessionID is the Claude session identifier (TaskEventInit only).
	SessionID string

	// DurationMs is the wall-clock duration of the run in milliseconds
	// (TaskEventResult only).
	DurationMs float64

	// CostUSD is the API cost of the run in US dollars (TaskEventResult only).
	CostUSD float64

	// Usage holds per-run token counts (TaskEventResult only).
	Usage Usage

	// IsError is true for TaskEventError events.
	IsError bool

	// ErrorMsg contains the error description when IsError is true.
	ErrorMsg string
}

// TaskStatus represents a task's lifecycle state.
type TaskStatus string

const (
	// TaskStatusIdle means the task is ready to accept a new Run call.
	TaskStatusIdle TaskStatus = "idle"

	// TaskStatusRunning means a prompt is currently being processed.
	// Calling Run returns an error until the current run completes.
	TaskStatusRunning TaskStatus = "running"

	// TaskStatusError means the last run ended with an error. The task
	// is still runnable — a subsequent Run call will retry.
	TaskStatusError TaskStatus = "error"

	// TaskStatusStopped means Stop was called. The task is permanently
	// terminated and Run will return an error.
	TaskStatusStopped TaskStatus = "stopped"
)

// TaskConfig holds the configuration for creating a Task.
type TaskConfig struct {
	// ID is the caller-assigned unique identifier for this task.
	ID string

	// Name is an optional human-readable label.
	Name string

	// WorkDir is the working directory passed to the claude process.
	WorkDir string

	// Model overrides the default model (e.g. "opus", "sonnet").
	// Empty means use Claude Code's default.
	Model string

	// ClaudeID is the claude session ID to resume with --resume. If
	// empty, each Run starts a fresh session. After the first run,
	// Task.ClaudeID() returns the session ID for subsequent calls.
	ClaudeID string

	// LastResult seeds Task.LastResult() before the first run — useful
	// when re-hydrating a task from persisted state.
	LastResult string
}

// RawLogFunc receives raw NDJSON lines from the Claude process.
type RawLogFunc func(line []byte)

// Task wraps a single headless Claude Code conversation using
// --output-format stream-json. Unlike [Agent] (which uses a
// persistent tmux session), Task spawns a new process per prompt
// and parses structured NDJSON events from stdout.
type Task struct {
	id      string
	name    string
	workDir string
	model   string

	mu         sync.Mutex
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	status     TaskStatus
	lastResult string
	claudeID   string
	onRawLog   RawLogFunc
}

// NewTask creates a Task from cfg. The task starts in [TaskStatusIdle] and
// is ready for its first [Task.Run] call immediately.
func NewTask(cfg TaskConfig) *Task {
	return &Task{
		id:         cfg.ID,
		name:       cfg.Name,
		workDir:    cfg.WorkDir,
		model:      cfg.Model,
		status:     TaskStatusIdle,
		claudeID:   cfg.ClaudeID,
		lastResult: cfg.LastResult,
	}
}

// ID returns the task's unique identifier.
func (t *Task) ID() string { return t.id }

// Name returns the task's human-readable name.
func (t *Task) Name() string { return t.name }

// WorkDir returns the task's working directory.
func (t *Task) WorkDir() string { return t.workDir }

// Status returns the current task status.
func (t *Task) Status() TaskStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// LastResult returns the final result text from the most recent command.
func (t *Task) LastResult() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastResult
}

// ClaudeID returns the Claude session ID used with --resume on subsequent
// runs. It is empty until the first TaskEventInit event is received.
func (t *Task) ClaudeID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.claudeID
}

// SetRawLog sets the callback for raw NDJSON lines from the Claude process.
func (t *Task) SetRawLog(fn RawLogFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onRawLog = fn
}

// Run sends prompt to claude and returns a channel of [TaskEvent] values.
// The channel is closed when the process exits. Events arrive in order:
// one [TaskEventInit], zero or more [TaskEventText]/[TaskEventToolUse], and
// a final [TaskEventResult] or [TaskEventError].
//
// Run returns an error immediately if the task is already running or has
// been stopped. ctx cancellation kills the underlying process.
func (t *Task) Run(ctx context.Context, prompt string) (<-chan TaskEvent, error) {
	t.mu.Lock()
	if t.status == TaskStatusRunning {
		t.mu.Unlock()
		return nil, fmt.Errorf("task %s is busy", t.id)
	}
	if t.status == TaskStatusStopped {
		t.mu.Unlock()
		return nil, fmt.Errorf("task %s is stopped", t.id)
	}
	t.status = TaskStatusRunning
	t.mu.Unlock()

	cmdCtx, cancel := context.WithCancel(ctx)

	args := []string{
		"-p",
		"--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
	}

	t.mu.Lock()
	cid := t.claudeID
	t.mu.Unlock()

	if cid != "" {
		args = append(args, "--resume", cid)
	}
	if t.model != "" {
		args = append(args, "--model", t.model)
	}
	args = append(args, prompt)

	slog.Debug("spawning claude task", "task", t.id, "args", args)

	claudeBin, err := resolveClaudeBin()
	if err != nil {
		cancel()
		t.mu.Lock()
		t.status = TaskStatusError
		t.mu.Unlock()
		return nil, err
	}
	cmd := exec.CommandContext(cmdCtx, claudeBin, args...)
	cmd.Dir = t.workDir

	// Unset CLAUDECODE to avoid nested session detection.
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.mu.Lock()
		t.status = TaskStatusError
		t.mu.Unlock()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.mu.Lock()
		t.status = TaskStatusError
		t.mu.Unlock()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		t.mu.Lock()
		t.status = TaskStatusError
		t.mu.Unlock()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	t.mu.Lock()
	t.cmd = cmd
	t.cancel = cancel
	t.mu.Unlock()

	ch := make(chan TaskEvent, 16)

	// Log stderr in a separate goroutine.
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			slog.Debug("claude stderr", "task", t.id, "line", scanner.Text())
		}
	}()

	// Parse stdout NDJSON and send events.
	go func() {
		defer close(ch)
		defer func() {
			if err := cmd.Wait(); err != nil {
				slog.Warn("claude process exited with error", "task", t.id, "error", err)
			} else {
				slog.Debug("claude process exited cleanly", "task", t.id)
			}
			cancel()
			t.mu.Lock()
			t.cmd = nil
			t.cancel = nil
			if t.status == TaskStatusRunning {
				t.status = TaskStatusIdle
			}
			t.mu.Unlock()
		}()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			t.mu.Lock()
			rawFn := t.onRawLog
			t.mu.Unlock()
			if rawFn != nil {
				rawFn(line)
			}

			events := ParseTaskLine(line)
			for _, ev := range events {
				switch ev.Type {
				case TaskEventInit:
					t.mu.Lock()
					if t.claudeID == "" {
						t.claudeID = ev.SessionID
						slog.Info("task session established",
							"task", t.id, "claude_id", ev.SessionID)
					}
					t.mu.Unlock()
				case TaskEventResult:
					t.mu.Lock()
					t.lastResult = ev.Content
					t.mu.Unlock()
				case TaskEventError:
					t.mu.Lock()
					t.lastResult = "error: " + ev.ErrorMsg
					t.mu.Unlock()
				}

				select {
				case ch <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// Cancel sends SIGINT to the running claude process, requesting a graceful
// stop. It is a no-op if no process is running. To permanently stop the task
// use [Task.Stop].
func (t *Task) Cancel() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	return t.cmd.Process.Signal(syscall.SIGINT)
}

// Stop permanently terminates the task. Any running process is killed and
// the status transitions to [TaskStatusStopped]. Subsequent Run calls return
// an error. Stop is idempotent.
func (t *Task) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = TaskStatusStopped
	if t.cancel != nil {
		t.cancel()
	}
}

// ParseTaskLine parses a single NDJSON line from claude's --output-format
// stream-json output and returns zero or more [TaskEvent] values. Lines that
// are unrecognised or malformed return nil. Callers that want to log raw lines
// before parsing can use [Task.SetRawLog].
func ParseTaskLine(line []byte) []TaskEvent {
	var base struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		return nil
	}

	switch base.Type {
	case "system":
		return parseTaskSystem(line)
	case "assistant":
		return parseTaskAssistant(line)
	case "result":
		return parseTaskResult(line)
	default:
		return nil
	}
}

func parseTaskSystem(line []byte) []TaskEvent {
	var msg struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}
	return []TaskEvent{{
		Type:      TaskEventInit,
		SessionID: msg.SessionID,
	}}
}

func parseTaskAssistant(line []byte) []TaskEvent {
	var msg struct {
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}

	var events []TaskEvent
	for _, raw := range msg.Message.Content {
		var block struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}

		switch block.Type {
		case "text":
			if block.Text != "" {
				events = append(events, TaskEvent{
					Type:    TaskEventText,
					Content: block.Text,
				})
			}
		case "tool_use":
			inputStr := ""
			if block.Input != nil {
				inputStr = string(block.Input)
			}
			events = append(events, TaskEvent{
				Type:      TaskEventToolUse,
				ToolID:    block.ID,
				ToolName:  block.Name,
				ToolInput: inputStr,
			})
		}
	}
	return events
}

func parseTaskResult(line []byte) []TaskEvent {
	var msg struct {
		Subtype      string  `json:"subtype"`
		Result       string  `json:"result"`
		DurationMs   float64 `json:"duration_ms"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        Usage   `json:"usage"`
		Errors       []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}

	if msg.Subtype == "success" {
		return []TaskEvent{{
			Type:       TaskEventResult,
			Content:    msg.Result,
			DurationMs: msg.DurationMs,
			CostUSD:    msg.TotalCostUSD,
			Usage:      msg.Usage,
		}}
	}

	var errMsgs []string
	for _, e := range msg.Errors {
		errMsgs = append(errMsgs, e.Message)
	}
	return []TaskEvent{{
		Type:     TaskEventError,
		IsError:  true,
		ErrorMsg: strings.Join(errMsgs, "; "),
	}}
}

// resolveClaudeBin locates the `claude` executable. It honours the
// CLAUDE_BIN env var (absolute path or PATH-resolvable name), then
// tries exec.LookPath, then falls back to common install locations.
// This matters when the parent process runs under a launcher (launchd,
// systemd, Windows Service) whose PATH excludes the user-local install
// dirs where Claude Code usually lives — e.g. ~/.local/bin/claude.
func resolveClaudeBin() (string, error) {
	if p := os.Getenv("CLAUDE_BIN"); p != "" {
		if filepath.IsAbs(p) {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		} else if abs, err := exec.LookPath(p); err == nil {
			return abs, nil
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("claude executable not found in PATH or known install dirs (set CLAUDE_BIN to override)")
}

func filterEnv(env []string, exclude string) []string {
	prefix := exclude + "="
	var result []string
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			result = append(result, e)
		}
	}
	return result
}

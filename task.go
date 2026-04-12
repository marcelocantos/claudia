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
	"strings"
	"sync"
	"syscall"
)

// TaskEventType identifies the kind of task event.
type TaskEventType string

const (
	TaskEventInit    TaskEventType = "init"
	TaskEventText    TaskEventType = "text"
	TaskEventToolUse TaskEventType = "tool_use"
	TaskEventResult  TaskEventType = "result"
	TaskEventError   TaskEventType = "error"
)

// Usage holds token counts from a result event.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// TaskEvent is a parsed event from Claude Code's stream-json output.
type TaskEvent struct {
	Type       TaskEventType
	Content    string  // text content or result text
	ToolName   string  // for tool_use events
	ToolInput  string  // for tool_use events (JSON string)
	ToolID     string  // for tool_use events
	SessionID  string  // for init events
	DurationMs float64 // for result events
	CostUSD    float64 // for result events
	Usage      Usage   // for result events
	IsError    bool    // true if result is an error
	ErrorMsg   string  // error message if IsError
}

// TaskStatus represents a task's lifecycle state.
type TaskStatus string

const (
	TaskStatusIdle    TaskStatus = "idle"
	TaskStatusRunning TaskStatus = "running"
	TaskStatusError   TaskStatus = "error"
	TaskStatusStopped TaskStatus = "stopped"
)

// TaskConfig holds the configuration for creating a task session.
type TaskConfig struct {
	ID       string // unique session ID
	Name     string // human-readable name
	WorkDir  string // working directory for claude process
	Model    string // model name (e.g. "opus", "sonnet"); empty = default
	ClaudeID string // claude session ID for --resume (empty = new session)
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

// NewTask creates a Task from a TaskConfig.
func NewTask(cfg TaskConfig) *Task {
	return &Task{
		id:       cfg.ID,
		name:     cfg.Name,
		workDir:  cfg.WorkDir,
		model:    cfg.Model,
		status:   TaskStatusIdle,
		claudeID: cfg.ClaudeID,
	}
}

// TaskID returns the task's unique identifier.
func (t *Task) TaskID() string { return t.id }

// TaskName returns the task's human-readable name.
func (t *Task) TaskName() string { return t.name }

// TaskWorkDir returns the task's working directory.
func (t *Task) TaskWorkDir() string { return t.workDir }

// TaskStatus returns the current task status.
func (t *Task) TaskStatus() TaskStatus {
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

// ClaudeID returns the Claude session ID (for --resume).
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

// SetLastResult sets the last result (used when restoring from DB).
func (t *Task) SetLastResult(r string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastResult = r
}

// RunTask sends a prompt to the task and returns a channel of events.
// The channel is closed when the process exits.
func (t *Task) RunTask(ctx context.Context, prompt string) (<-chan TaskEvent, error) {
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

	cmd := exec.CommandContext(cmdCtx, "claude", args...)
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

// CancelTask sends SIGINT to the running claude process.
func (t *Task) CancelTask() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	return t.cmd.Process.Signal(syscall.SIGINT)
}

// StopTask terminates the task permanently.
func (t *Task) StopTask() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = TaskStatusStopped
	if t.cancel != nil {
		t.cancel()
	}
}

// ParseTaskLine parses a single NDJSON line from claude's stream-json
// output and returns zero or more TaskEvents.
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

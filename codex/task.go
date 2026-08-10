// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// Status is the lifecycle state of a [Task].
type Status string

const (
	// StatusIdle means the task is ready for Run.
	StatusIdle Status = "idle"
	// StatusRunning means a prompt is in flight.
	StatusRunning Status = "running"
	// StatusError means the last run ended with an error; Run may retry.
	StatusError Status = "error"
	// StatusStopped means Stop was called; Run will fail.
	StatusStopped Status = "stopped"
)

// Config holds configuration for a Codex Task.
// Prefer a struct over functional options.
type Config struct {
	// ID is an optional caller-assigned identifier.
	ID string
	// Name is an optional human-readable label.
	Name string
	// WorkDir is the working directory (--cd) for codex exec.
	WorkDir string
	// Model overrides the Codex model (--model).
	Model string
	// SandboxMode is passed as --sandbox (e.g. "read-only").
	SandboxMode string
	// ApprovalPolicy is passed as --ask-for-approval (e.g. "never").
	ApprovalPolicy string
	// SessionID resumes a prior Codex thread via `codex exec resume`.
	SessionID string
	// Resolve customises binary resolution and auth preflight (tests).
	Resolve *ResolveArgs
	// RawLog receives raw NDJSON lines before parsing (optional).
	RawLog func(line []byte)
}

// Task is a headless Codex agent driven by `codex exec --json`.
type Task struct {
	id       string
	name     string
	workDir  string
	model    string
	sandbox  string
	approval string
	resolve  *ResolveArgs
	rawLog   func(line []byte)

	mu         sync.Mutex
	status     Status
	sessionID  string
	lastResult string
	cancel     context.CancelFunc
}

// NewTask creates a Codex Task in [StatusIdle].
func NewTask(cfg Config) *Task {
	return &Task{
		id:        cfg.ID,
		name:      cfg.Name,
		workDir:   cfg.WorkDir,
		model:     cfg.Model,
		sandbox:   cfg.SandboxMode,
		approval:  cfg.ApprovalPolicy,
		resolve:   cfg.Resolve,
		rawLog:    cfg.RawLog,
		status:    StatusIdle,
		sessionID: cfg.SessionID,
	}
}

// NewCodexTask is an alias for [NewTask] for callers who want the explicit name.
func NewCodexTask(cfg Config) *Task { return NewTask(cfg) }

// ID returns the caller-assigned identifier.
func (t *Task) ID() string { return t.id }

// Name returns the human-readable label.
func (t *Task) Name() string { return t.name }

// WorkDir returns the configured working directory.
func (t *Task) WorkDir() string { return t.workDir }

// Status returns the current lifecycle status.
func (t *Task) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// SessionID returns the Codex thread id captured from thread.started
// (or the seed from Config.SessionID before the first successful init).
func (t *Task) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// LastResult returns the final agent message from the most recent successful run.
func (t *Task) LastResult() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastResult
}

// Stop permanently terminates the task. A running process is cancelled.
func (t *Task) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = StatusStopped
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
}

// Run spawns `codex exec --json` for prompt and returns a channel of
// structured [Event] values. The channel is closed when the process exits.
//
// Preflight: refuses to spawn when ChatGPT-subscription auth is not active
// ([*AuthError]). Non-zero process exit without a prior structured error
// yields an [EventError] carrying [*ExitError].
func (t *Task) Run(ctx context.Context, prompt string) (<-chan Event, error) {
	t.mu.Lock()
	if t.status == StatusStopped {
		t.mu.Unlock()
		return nil, fmt.Errorf("codex task stopped")
	}
	if t.status == StatusRunning {
		t.mu.Unlock()
		return nil, fmt.Errorf("codex task already running")
	}
	t.status = StatusRunning
	runCtx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	sessionID := t.sessionID
	t.mu.Unlock()

	if err := ensureSubscriptionAuth(t.resolve); err != nil {
		t.finish(StatusError)
		return nil, err
	}

	bin, err := resolveBin(t.resolve)
	if err != nil {
		t.finish(StatusError)
		return nil, err
	}

	args := execArgs(execArgInput{
		WorkDir:        t.workDir,
		Model:          t.model,
		SandboxMode:    t.sandbox,
		ApprovalPolicy: t.approval,
		SessionID:      sessionID,
		Prompt:         prompt,
	})
	slog.Debug("codex task spawn", "bin", bin, "args", args)

	cmd := exec.CommandContext(runCtx, bin, args...)
	if t.workDir != "" {
		cmd.Dir = t.workDir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.finish(StatusError)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.finish(StatusError)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		t.finish(StatusError)
		return nil, fmt.Errorf("start codex: %w", err)
	}

	var stderrBuf strings.Builder
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 256*1024), 256*1024)
		for sc.Scan() {
			line := sc.Text()
			slog.Debug("codex stderr", "line", line)
			if stderrBuf.Len() < 8*1024 {
				stderrBuf.WriteString(line)
				stderrBuf.WriteByte('\n')
			}
		}
	}()

	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		var (
			p          parser
			sawError   bool
			sawResult  bool
			lastResult string
			initID     string
		)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			if t.rawLog != nil {
				// Copy: scanner reuses the buffer.
				cp := append([]byte(nil), line...)
				t.rawLog(cp)
			}
			for _, ev := range p.ParseLine(line) {
				switch ev.Type {
				case EventInit:
					if ev.SessionID != "" {
						initID = ev.SessionID
						t.mu.Lock()
						t.sessionID = ev.SessionID
						t.mu.Unlock()
					}
				case EventResult:
					sawResult = true
					lastResult = ev.Content
				case EventError:
					sawError = true
				}
				select {
				case ch <- ev:
				case <-runCtx.Done():
					t.finish(StatusError)
					return
				}
			}
		}

		waitErr := cmd.Wait()
		if waitErr != nil && !sawError {
			exitCode := 1
			if ee, ok := waitErr.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			}
			msg := strings.TrimSpace(stderrBuf.String())
			if msg == "" {
				msg = waitErr.Error()
			}
			// If stderr looks like auth/rate-limit, classify; else ExitError.
			var typed error
			if isAuthMessage(strings.ToLower(msg)) || isRateLimitMessage(strings.ToLower(msg)) {
				typed = ClassifyFailure(msg)
			} else {
				typed = &ExitError{ExitCode: exitCode, Message: msg}
			}
			sawError = true
			select {
			case ch <- Event{Type: EventError, Error: typed}:
			case <-runCtx.Done():
			}
		}

		if initID != "" {
			t.mu.Lock()
			t.sessionID = initID
			t.mu.Unlock()
		}
		if sawResult {
			t.mu.Lock()
			t.lastResult = lastResult
			t.mu.Unlock()
		}
		if sawError {
			t.finish(StatusError)
		} else {
			t.finish(StatusIdle)
		}
	}()

	return ch, nil
}

func (t *Task) finish(st Status) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status == StatusStopped {
		return
	}
	t.status = st
	t.cancel = nil
}

type execArgInput struct {
	WorkDir        string
	Model          string
	SandboxMode    string
	ApprovalPolicy string
	SessionID      string
	Prompt         string
}

// execArgs builds argv for codex. Global flags belong before `exec`
// (CLI 0.146.0-alpha.x rejects --ask-for-approval after the subcommand).
func execArgs(in execArgInput) []string {
	args := []string{}
	if in.ApprovalPolicy != "" {
		args = append(args, "--ask-for-approval", in.ApprovalPolicy)
	}
	if in.WorkDir != "" {
		args = append(args, "--cd", in.WorkDir)
	}
	if in.SandboxMode != "" {
		args = append(args, "--sandbox", in.SandboxMode)
	}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	args = append(args, "exec")
	if in.SessionID != "" {
		args = append(args, "resume", "--json", in.SessionID, in.Prompt)
	} else {
		args = append(args, "--json", in.Prompt)
	}
	return args
}

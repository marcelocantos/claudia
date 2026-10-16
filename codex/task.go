// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"context"
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
	// StatusCancelled means the run's context ended the task. Distinct
	// from StatusError, which is an ordinary non-zero exit or a
	// structured provider error, and from StatusStopped, which is Stop.
	// Run may retry.
	StatusCancelled Status = "cancelled"
	// StatusStopped means Stop was called; Run will fail.
	StatusStopped Status = "stopped"
)

// Config preserves source compatibility for the removed Codex Task API.
// Only ID, Name, WorkDir and SessionID are retained on the inert handle;
// all execution settings are unused and Run returns ErrTaskCLIRemoved.
type Config struct {
	// ID is an optional caller-assigned identifier.
	ID string
	// Name is an optional human-readable label.
	Name string
	// WorkDir is retained as handle metadata.
	WorkDir string
	// Model is retained for source compatibility; no model runs.
	Model string
	// SandboxMode is unused after CLI Task removal.
	SandboxMode string
	// SandboxGitWrite opts a workspace-write task into writing its Git metadata.
	// This includes hooks and config, which execute outside Codex's sandbox on
	// later Git commands. Leave unset unless the task must commit.
	SandboxGitWrite bool
	// ApprovalPolicy is unused after CLI Task removal.
	ApprovalPolicy string
	// SessionID is retained as handle metadata; no session is resumed.
	SessionID string
	// Resolve is unused; Run performs no binary or credential discovery.
	Resolve *ResolveArgs
	// RawLog is unused; Run produces no provider output.
	RawLog func(line []byte)
}

// Task preserves the removed Codex CLI Task API. Run refuses with ErrTaskCLIRemoved.
type Task struct {
	id      string
	name    string
	workDir string

	mu         sync.Mutex
	status     Status
	sessionID  string
	lastResult string
}

// NewTask creates a Codex Task in [StatusIdle].
func NewTask(cfg Config) *Task {
	return &Task{
		id:        cfg.ID,
		name:      cfg.Name,
		workDir:   cfg.WorkDir,
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

// SessionID returns the metadata supplied in Config.SessionID.
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

// Stop marks the inert task stopped. No process exists.
func (t *Task) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = StatusStopped
}

// Run always returns ErrTaskCLIRemoved. The CLI execution transport has
// been removed; this compatibility API does not start a replacement agent.
func (t *Task) Run(context.Context, string) (<-chan Event, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != StatusStopped {
		t.status = StatusError
	}
	return nil, ErrTaskCLIRemoved
}

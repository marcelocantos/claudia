// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

	// TaskEventToolUse is emitted when a provider invokes a tool. The tool
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
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CacheCreationInputTokens is tokens written to the prompt cache.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// CacheReadInputTokens is tokens read from the prompt cache (billed
	// at a lower rate than uncached input tokens).
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// TaskEvent is a parsed event from a provider's headless task stream.
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

	// Model is the model the backend resolved for the run (TaskEventInit),
	// e.g. "claude-opus-5" — the full id even when an alias was requested.
	// Compare it against the requested model to detect a silent fallback.
	Model string

	// Truncated reports that this event's payload was bounded before it was
	// relayed over the broker wire because it was too large for one frame;
	// ToolInput is the field a base64 screenshot arrives in. The elided
	// strings carry a marker naming the bytes dropped. Direct (non-brokered)
	// runs never set it (🎯T73).
	Truncated bool
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

// BaseDisallowedTools are the tools no claudia-spawned Claude agent may
// use in the legacy Session transport. It is applied on Claude only; the
// names are Claude Code tools no other provider implements.
//
// Agents spawned by claudia are forbidden from creating their own
// sub-agents: the host program owns the process lifecycle, and an agent
// that can spawn agents takes that ownership away. The cost of getting
// this wrong is not abstract — a summariser handed a transcript
// containing "go deep with fanout" obeyed it and produced ~33,000
// subagents and ~4.3 billion tokens in two hours.
//
// TeamCreate and TeamDelete used to be listed too. Claude Code no longer
// ships those tools, and a deny rule that matches no tool is printed as a
// startup warning ("Permission deny rule \"TeamCreate\" matches no known
// tool") on every launch — a frame that, seen alone, looks like a seat that
// never drew its composer (jevons 🎯T565). Nothing is lost by dropping them:
// a tool that does not exist cannot be called.
const BaseDisallowedTools = "Agent,SendMessage,EnterWorktree"

// disallowedToolList joins the always-forbidden baseline with any extra
// tools the caller wants removed.
func disallowedToolList(extra []string) string {
	disallowed := BaseDisallowedTools
	if len(extra) > 0 {
		disallowed += "," + strings.Join(extra, ",")
	}
	return disallowed
}

// TaskConfig holds configuration for a one-shot Bedrock or Ollama API request.
// SessionID and its deprecated ClaudeID alias remain for source compatibility;
// these stateless API providers refuse non-empty session identifiers.
// CLI-only fields remain for source compatibility, but no longer enable execution:
// default, Claude, Codex, Grok and Cursor return [ErrTaskCLIRemoved].
type TaskConfig struct {
	// ID is the caller-assigned unique identifier for this task.
	ID string

	// Name is an optional human-readable label.
	Name string

	// Provider selects the runtime backing this task. Empty means
	// ProviderClaude.
	Provider Provider

	// RequireBroker refuses the run if the host daemon cannot accept it.
	// It prevents the usual direct-API fallback when the broker is
	// absent or loses its runtime. Use it when admission and host-wide
	// accounting must never be bypassed.
	RequireBroker bool

	// WorkDir is the caller working directory (absolute for broker requests).
	WorkDir string

	// Model selects the API model. Empty uses provider-specific configuration.
	Model string

	// SandboxMode, SandboxGitWrite, ApprovalPolicy, DisallowTools, ToolPolicy
	// and SessionID are retained for compatibility with removed CLI Tasks.
	// Bedrock and Ollama refuse nonzero unsupported settings.
	SandboxMode     string
	SandboxGitWrite bool
	ApprovalPolicy  string
	DisallowTools   []string
	ToolPolicy      *TaskToolPolicy
	SessionID       string

	// ClaudeID is the pre-1.0 name of SessionID. It was never Claude-only:
	// every provider's resume handle went through it. When both are set,
	// SessionID wins; when only ClaudeID is set, it is honoured unchanged.
	//
	// Deprecated: use SessionID. The field stays until the 1.0 breaking
	// cut (🎯T1) so existing callers keep working.
	ClaudeID string

	// LastResult seeds Task.LastResult() before the first run — useful
	// when re-hydrating a task from persisted state.
	LastResult string

	// PickByRemaining is retained for compatibility. The subscription CLI Task
	// providers have been removed, so a default-provider run always refuses.
	PickByRemaining bool
}

// resumeID is the session Run continues: SessionID when set, else the
// deprecated ClaudeID. Every consumer of the handle reads it through here
// so the two spellings cannot disagree.
func (cfg TaskConfig) resumeID() string {
	if cfg.SessionID != "" {
		return cfg.SessionID
	}
	return cfg.ClaudeID
}

// TaskToolPolicy retains the removed CLI Task allowlist wire shape.
// No remaining public Task backend accepts this policy. It does not
// configure sidecar seats or enable an OMP Task replacement.
type TaskToolPolicy struct {
	Builtins []string `json:"builtins"`
	Allow    []string `json:"allow,omitempty"`
	Deny     []string `json:"deny,omitempty"`
	MaxTurns int      `json:"max_turns"`
	HomeDir  string   `json:"home_dir,omitempty"`
}

func validateTaskToolPolicy(provider Provider, policy *TaskToolPolicy) error {
	if policy == nil {
		return nil
	}
	if provider == "" {
		provider = ProviderClaude
	}
	if provider != ProviderClaude && provider != ProviderGrok {
		return unsupportedCapability(provider, CapabilityToolRestrictions,
			"TaskToolPolicy is supported only by Claude and Grok headless tasks")
	}
	if len(policy.Builtins) == 0 || policy.MaxTurns < 1 {
		return fmt.Errorf("task tool policy requires builtins and a positive max turns")
	}
	for _, builtin := range policy.Builtins {
		if builtin == "" || strings.ContainsAny(builtin, ", \t\r\n") {
			return fmt.Errorf("invalid task tool name %q", builtin)
		}
	}
	for _, rule := range append(append([]string(nil), policy.Allow...), policy.Deny...) {
		if strings.TrimSpace(rule) == "" || strings.ContainsAny(rule, "\r\n") {
			return fmt.Errorf("invalid task permission rule %q", rule)
		}
	}
	if policy.HomeDir != "" {
		if provider != ProviderGrok {
			return fmt.Errorf("task tool policy home is supported only for Grok")
		}
		if !filepath.IsAbs(policy.HomeDir) {
			return fmt.Errorf("task tool policy home must be absolute")
		}
		info, err := os.Lstat(policy.HomeDir)
		if err != nil {
			return fmt.Errorf("task tool policy home: %w", err)
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("task tool policy home %s must be a private directory", policy.HomeDir)
		}
	} else if provider == ProviderGrok {
		return fmt.Errorf("restricted Grok task requires an isolated home")
	}
	return nil
}

// RawLogFunc receives raw provider stream records when a backend supplies them.
type RawLogFunc func(line []byte)

// Task runs a one-shot Bedrock or Ollama API request. The removed CLI
// providers, including the empty default, return [ErrTaskCLIRemoved].
// There is no OMP Task replacement (T190).
type Task struct {
	id            string
	name          string
	provider      Provider
	requireBroker bool
	workDir       string
	model         string
	sandbox       string
	gitWrite      bool
	approval      string
	disallow      []string
	toolPolicy    *TaskToolPolicy
	pickRemaining bool

	mu            sync.Mutex
	run           *taskRun
	cancel        context.CancelFunc
	status        TaskStatus
	lastResult    string
	claudeID      string
	resolvedModel string
	onRawLog      RawLogFunc
	backend       taskBackend
	// direct skips the broker consult: the daemon's own runs.
	direct bool
}

type taskRunRequest struct {
	WorkDir         string
	Model           string
	SandboxMode     string
	SandboxGitWrite bool
	ApprovalPolicy  string
	DisallowTools   []string
	ToolPolicy      *TaskToolPolicy
	SessionID       string
	Prompt          string
	RawLog          RawLogFunc
}

type taskRun struct {
	events    <-chan TaskEvent
	interrupt func() error
}

type taskBackend interface {
	Capabilities() providerCapabilities
	RunTask(context.Context, taskRunRequest) (*taskRun, error)
}

type errorTaskBackend struct {
	err error
}

func taskBackendForProvider(provider Provider) taskBackend {
	switch provider {
	case "", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor, "anthropic", "openai-codex", "xai-oauth":
		return errorTaskBackend{err: fmt.Errorf("%w: provider %q", ErrTaskCLIRemoved, provider)}
	case ProviderBedrock:
		return bedrockTaskBackend{}
	case ProviderOllama:
		return ollamaTaskBackend{}
	default:
		return errorTaskBackend{err: fmt.Errorf("unknown task provider %q", provider)}
	}
}

// TaskProviderError reports a provider whose Task transport is unavailable.
// Session support is independent; the four subscription plans still run seats.
func TaskProviderError(provider Provider) error {
	if b, ok := taskBackendForProvider(provider).(errorTaskBackend); ok {
		return b.err
	}
	return nil
}

func (b errorTaskBackend) Capabilities() providerCapabilities {
	return providerCapabilities{}
}

func (b errorTaskBackend) RunTask(context.Context, taskRunRequest) (*taskRun, error) {
	return nil, b.err
}

// NewTask creates a Task from cfg. The task starts in [TaskStatusIdle] and
// is ready for its first [Task.Run] call immediately.
func NewTask(cfg TaskConfig) *Task {
	return newTaskWithBackend(cfg, taskBackendForProvider(cfg.Provider))
}

func newTaskWithBackend(cfg TaskConfig, backend taskBackend) *Task {
	return &Task{
		id:            cfg.ID,
		name:          cfg.Name,
		workDir:       cfg.WorkDir,
		model:         cfg.Model,
		provider:      cfg.Provider,
		requireBroker: cfg.RequireBroker,
		sandbox:       cfg.SandboxMode,
		gitWrite:      cfg.SandboxGitWrite,
		approval:      cfg.ApprovalPolicy,
		disallow:      cfg.DisallowTools,
		toolPolicy:    cfg.ToolPolicy,
		pickRemaining: cfg.PickByRemaining,
		status:        TaskStatusIdle,
		claudeID:      cfg.resumeID(),
		lastResult:    cfg.LastResult,
		backend:       backend,
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

// SessionID returns the provider session id Run resumes on subsequent
// runs: the TaskConfig.SessionID the task was created with, or the id the
// provider reported in its first TaskEventInit. It is empty until one of
// those has happened. The same value is returned for every provider.
func (t *Task) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.claudeID
}

// ClaudeID returns the same session id as SessionID. The name predates
// multi-provider Task support; the id is Claude's only when the provider is.
//
// Deprecated: use SessionID. The method stays until the 1.0 breaking cut
// (🎯T1) so existing callers keep working.
func (t *Task) ClaudeID() string {
	return t.SessionID()
}

// Model returns the model the backend resolved for this task, captured from
// the init event (the full id even when an alias was requested). It is empty
// until the first TaskEventInit arrives. Compare it against the requested
// model to detect a silent fallback.
func (t *Task) Model() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resolvedModel
}

// SetDirect makes this Task run in this process even when a claudia daemon
// is listening (true), or consult the daemon as usual (false, the default).
// Unlike CLAUDIA_NO_BROKER it affects only this Task.
func (t *Task) SetDirect(direct bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.direct = direct
}

// SetRawLog sets the callback for raw provider stream records.
func (t *Task) SetRawLog(fn RawLogFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onRawLog = fn
}

// Run starts a one-shot API request and returns its ordered event stream.
// The channel closes when the request finishes. Removed CLI providers return
// ErrTaskCLIRemoved before broker lookup or dispatch. Running or stopped tasks
// refuse another Run; context cancellation cancels the underlying request.
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
	// Refuse removed transports before any broker lookup, policy check, or
	// provider discovery. An older broker must not resurrect the CLI path.
	if b, ok := t.backend.(errorTaskBackend); ok {
		t.status = TaskStatusError
		t.mu.Unlock()
		return nil, b.err
	}
	t.status = TaskStatusRunning
	t.mu.Unlock()
	if err := validateTaskToolPolicy(t.provider, t.toolPolicy); err != nil {
		t.mu.Lock()
		t.status = TaskStatusError
		t.mu.Unlock()
		return nil, err
	}

	cmdCtx, cancel := context.WithCancel(ctx)

	t.mu.Lock()
	cid := t.claudeID
	rawFn := t.onRawLog
	t.mu.Unlock()

	req := taskRunRequest{
		WorkDir:         t.workDir,
		Model:           t.model,
		SandboxMode:     t.sandbox,
		SandboxGitWrite: t.gitWrite,
		ApprovalPolicy:  t.approval,
		DisallowTools:   t.disallow,
		ToolPolicy:      t.toolPolicy,
		SessionID:       cid,
		Prompt:          prompt,
		RawLog:          rawFn,
	}
	var run *taskRun
	var err error
	// A listening daemon owns the run (🎯T2.10). By default a bare protocol
	// server or absent socket falls through to a direct API request. Callers that
	// require host-wide admission explicitly refuse that fallback.
	var bb *brokerTaskBackend
	t.mu.Lock()
	direct := t.direct
	t.mu.Unlock()
	if !direct {
		bb = taskBackendConsideringBroker(TaskConfig{
			ID: t.id, Name: t.name, Provider: t.provider, PickByRemaining: t.pickRemaining,
		})
	}
	if bb != nil {
		run, err = bb.RunTask(cmdCtx, req)
		if err != nil && brokerFellThrough(err) {
			bb.client.Close()
			switch {
			case t.requireBroker:
				err = fmt.Errorf("%w: %v", ErrBrokerRequired, err)
			case t.pickRemaining:
				err = fmt.Errorf("pick by remaining requires the broker: %w", err)
			default:
				run, err = t.backend.RunTask(cmdCtx, req)
			}
		} else if err != nil {
			bb.client.Close()
		}
	} else if t.pickRemaining {
		err = fmt.Errorf("pick by remaining requires the broker: %w", ErrNoBroker)
	} else {
		if t.requireBroker {
			if direct {
				err = fmt.Errorf("%w: direct mode selected", ErrBrokerRequired)
			} else {
				err = fmt.Errorf("%w: no daemon at %s", ErrBrokerRequired, brokerSocketPath())
			}
		} else {
			run, err = t.backend.RunTask(cmdCtx, req)
		}
	}
	if err != nil {
		cancel()
		t.mu.Lock()
		t.status = TaskStatusError
		t.mu.Unlock()
		return nil, err
	}

	t.mu.Lock()
	t.run = run
	t.cancel = cancel
	t.mu.Unlock()

	ch := make(chan TaskEvent, 16)

	go func() {
		defer close(ch)
		defer func() {
			cancel()
			t.mu.Lock()
			t.run = nil
			t.cancel = nil
			if t.status == TaskStatusRunning {
				t.status = TaskStatusIdle
			}
			t.mu.Unlock()
		}()

		for ev := range run.events {
			t.recordTaskEvent(ev)
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// Cancel interrupts the current request, if any. To permanently stop the task
// use [Task.Stop].
func (t *Task) Cancel() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.run == nil || t.run.interrupt == nil {
		return nil
	}
	return t.run.interrupt()
}

// Stop permanently terminates the task. Any running request is cancelled and
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

func (t *Task) recordTaskEvent(ev TaskEvent) {
	switch ev.Type {
	case TaskEventInit:
		t.mu.Lock()
		if t.claudeID == "" {
			t.claudeID = ev.SessionID
			slog.Info("task session established",
				"task", t.id, "claude_id", ev.SessionID)
		}
		if ev.Model != "" && t.resolvedModel == "" {
			t.resolvedModel = ev.Model
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
}

// forwardTaskStream reads NDJSON lines from r, hands each raw line to
// rawLog, and publishes the parsed events on ch until r is exhausted or
// ctx is cancelled. Blank lines are skipped; the byte slice handed to
// rawLog and parse is the scanner's, valid only for that iteration.
//
// It is the one place every process-backed provider honours the caller's
// RawLog callback, which is also what lets the request-field audit prove
// RawLog is honoured without spawning a process: the audit feeds a
// synthetic wire line through this function rather than asserting that
// somebody remembered to call it.
func forwardTaskStream(
	ctx context.Context,
	r io.Reader,
	parse func([]byte) []TaskEvent,
	rawLog RawLogFunc,
	ch chan<- TaskEvent,
) error {
	// 🎯T133: a single NDJSON line can carry an embedded large tool
	// payload (e.g. an image read back through Read) that exceeds a
	// small fixed scanner buffer. bufio.Scanner reports that as
	// bufio.ErrTooLong and stops silently unless the caller checks
	// scanner.Err() — the caller previously did not, so a Claude Task
	// run whose final result line happened to be oversized vanished
	// with no TaskEventResult and no error ("claudia task: no final
	// result", undiagnosable). Grow the buffer well past the prior 1
	// MiB cap so ordinary large-but-sane lines scan cleanly, and
	// surface any remaining scan failure (still-too-long or read
	// error) to the caller instead of discarding it.
	const maxTaskLine = 64 * 1024 * 1024 // 64 MiB
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), maxTaskLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if rawLog != nil {
			rawLog(line)
		}
		for _, ev := range parse(line) {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning task stream: %w", err)
	}
	return nil
}

// grokTaskParser maps Grok Build headless streaming-json lines to TaskEvent.
//
// A usage event ends each model response, including responses that precede a
// tool call. Only the last response is the turn's result; earlier text is
// progress, not part of the final answer. Session id arrives only on end.
type grokTaskParser struct {
	text         strings.Builder
	lastResponse string
}

func (p *grokTaskParser) Parse(line []byte) []TaskEvent {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		return nil
	}

	switch base.Type {
	case "text":
		var msg struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		p.text.WriteString(msg.Data)
		return []TaskEvent{{Type: TaskEventText, Content: msg.Data}}
	case "tool_call":
		var msg struct {
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			Title      string          `json:"title"`
			RawInput   json.RawMessage `json:"rawInput"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		name := msg.ToolName
		if name == "" {
			name = msg.Title
		}
		return []TaskEvent{{Type: TaskEventToolUse, ToolID: msg.ToolCallID, ToolName: name, ToolInput: string(msg.RawInput)}}
	case "thought":
		// Internal reasoning — not part of the provider-neutral result text.
		return nil
	case "usage":
		p.lastResponse = p.text.String()
		p.text.Reset()
		return nil
	case "end":
		var msg struct {
			SessionID  string `json:"sessionId"`
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		out := make([]TaskEvent, 0, 2)
		if msg.SessionID != "" {
			out = append(out, TaskEvent{Type: TaskEventInit, SessionID: msg.SessionID})
		}
		result := p.text.String()
		if result == "" {
			result = p.lastResponse
		}
		out = append(out, TaskEvent{
			Type:    TaskEventResult,
			Content: result,
		})
		return out
	case "error":
		var msg struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		return []TaskEvent{{Type: TaskEventError, IsError: true, ErrorMsg: msg.Message}}
	default:
		// max_turns_reached, auto_compact_*, unknown — ignore until mapped.
		return nil
	}
}

type codexTaskParser struct {
	lastAgentMessage string
	seenCommands     map[string]bool
}

func (p *codexTaskParser) Parse(line []byte) []TaskEvent {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		return nil
	}

	switch base.Type {
	case "thread.started":
		var msg struct {
			ThreadID string `json:"thread_id"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		return []TaskEvent{{Type: TaskEventInit, SessionID: msg.ThreadID}}
	case "item.started", "item.completed":
		return p.parseCodexItem(line)
	case "turn.completed":
		var msg struct {
			Usage struct {
				InputTokens           int `json:"input_tokens"`
				CachedInputTokens     int `json:"cached_input_tokens"`
				OutputTokens          int `json:"output_tokens"`
				ReasoningOutputTokens int `json:"reasoning_output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		return []TaskEvent{{
			Type:    TaskEventResult,
			Content: p.lastAgentMessage,
			Usage: Usage{
				InputTokens:          msg.Usage.InputTokens,
				OutputTokens:         msg.Usage.OutputTokens,
				CacheReadInputTokens: msg.Usage.CachedInputTokens,
			},
		}}
	case "turn.failed":
		var msg struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		return []TaskEvent{{Type: TaskEventError, IsError: true, ErrorMsg: msg.Error}}
	case "error":
		var msg struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil
		}
		errMsg := msg.Message
		if errMsg == "" {
			errMsg = msg.Error
		}
		return []TaskEvent{{Type: TaskEventError, IsError: true, ErrorMsg: errMsg}}
	default:
		return nil
	}
}

func (p *codexTaskParser) parseCodexItem(line []byte) []TaskEvent {
	var msg struct {
		Type string `json:"type"`
		Item struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Command string          `json:"command"`
			Status  string          `json:"status"`
			Raw     json.RawMessage `json:"-"`
		} `json:"item"`
	}
	var raw struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}
	if err := json.Unmarshal(line, &raw); err == nil {
		msg.Item.Raw = raw.Item
	}

	if msg.Type == "item.completed" && msg.Item.Type == "agent_message" && msg.Item.Text != "" {
		p.lastAgentMessage = msg.Item.Text
		return []TaskEvent{{Type: TaskEventText, Content: msg.Item.Text}}
	}
	if msg.Item.Type == "command_execution" {
		if msg.Item.ID != "" {
			if p.seenCommands[msg.Item.ID] {
				return nil
			}
			if p.seenCommands == nil {
				p.seenCommands = make(map[string]bool)
			}
			p.seenCommands[msg.Item.ID] = true
		}
		input := string(msg.Item.Raw)
		if input == "" {
			input = fmt.Sprintf(`{"command":%q,"status":%q}`, msg.Item.Command, msg.Item.Status)
		}
		return []TaskEvent{{
			Type:      TaskEventToolUse,
			ToolID:    msg.Item.ID,
			ToolName:  msg.Item.Type,
			ToolInput: input,
		}}
	}
	return nil
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
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}
	return []TaskEvent{{
		Type:      TaskEventInit,
		SessionID: msg.SessionID,
		Model:     msg.Model,
	}}
}

func parseTaskAssistant(line []byte) []TaskEvent {
	if msg, ok := claudeAssistantAPIError(line); ok {
		return []TaskEvent{{Type: TaskEventError, IsError: true, ErrorMsg: msg}}
	}
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

// TaskErrorSpendLimit prefixes a Task error whose provider refused the
// turn because the account's spend limit is reached.
const TaskErrorSpendLimit = "spend_limit"

// claudeAssistantAPIError reports a stream-json assistant line that
// carries Claude Code's top-level "error" class (rate_limit,
// billing_error, authentication_failed, ...). Claude Code emits the
// refusal as a synthetic assistant message — on 2026-10-12 it was
// error "rate_limit" with the text "You've hit your monthly spend limit
// · raise it at ... · your session limit resets 10:10pm" (apiError
// usage_limit_reached, overageStatus rejected) — and print mode then
// keeps the process alive while background tasks are pending, so no
// result line follows for half an hour. The refusal is the end of the
// run: it is returned as a Task error, with spend_limit named when the
// provider's own text says so.
func claudeAssistantAPIError(line []byte) (string, bool) {
	var msg struct {
		Error   string `json:"error"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil || msg.Error == "" {
		return "", false
	}
	var text []string
	for _, c := range msg.Message.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			text = append(text, strings.TrimSpace(c.Text))
		}
	}
	detail := strings.Join(text, " ")
	class := msg.Error
	if strings.Contains(strings.ToLower(detail), "spend limit") {
		class = TaskErrorSpendLimit
	}
	if detail == "" {
		detail = "provider refused the turn"
	}
	return "claude " + class + ": " + detail, true
}

func parseTaskResult(line []byte) []TaskEvent {
	var msg struct {
		Subtype      string  `json:"subtype"`
		Result       string  `json:"result"`
		IsError      bool    `json:"is_error"`
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

	// Live Claude shape (invalid --model): subtype stays "success" while
	// is_error is true and result names the offending model. Treating
	// subtype alone would swallow the failure as TaskEventResult.
	if msg.IsError || msg.Subtype != "success" {
		var errMsgs []string
		for _, e := range msg.Errors {
			if e.Message != "" {
				errMsgs = append(errMsgs, e.Message)
			}
		}
		errMsg := strings.Join(errMsgs, "; ")
		if errMsg == "" {
			errMsg = strings.TrimSpace(msg.Result)
		}
		if errMsg == "" {
			errMsg = "task failed"
		}
		return []TaskEvent{{
			Type:     TaskEventError,
			IsError:  true,
			ErrorMsg: errMsg,
		}}
	}

	return []TaskEvent{{
		Type:       TaskEventResult,
		Content:    msg.Result,
		DurationMs: msg.DurationMs,
		CostUSD:    msg.TotalCostUSD,
		Usage:      msg.Usage,
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

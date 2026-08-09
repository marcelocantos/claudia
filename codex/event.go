// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

// EventType identifies the kind of streaming event from a Codex Task run.
type EventType string

const (
	// EventInit is emitted when the Codex thread starts. SessionID holds
	// the thread id (resume handle for a later Run).
	EventInit EventType = "init"

	// EventText carries an agent_message text chunk.
	EventText EventType = "text"

	// EventToolUse is emitted for command_execution (and similar) items.
	EventToolUse EventType = "tool_use"

	// EventResult is the final successful turn result (last agent message
	// plus token usage when present).
	EventResult EventType = "result"

	// EventError is a structured failure. Error holds a typed error when
	// classification succeeded (*AuthError, *RateLimitError, *ExitError, or
	// a plain *RunError).
	EventError EventType = "error"
)

// Usage holds token counts from a turn.completed event.
type Usage struct {
	InputTokens          int
	OutputTokens         int
	CacheReadInputTokens int
}

// Event is a parsed event from `codex exec --json` output.
type Event struct {
	Type EventType

	// Content is text (EventText) or final result text (EventResult).
	Content string

	// ToolName, ToolID, ToolInput describe EventToolUse.
	ToolName  string
	ToolID    string
	ToolInput string

	// SessionID is the Codex thread id (EventInit).
	SessionID string

	// Usage is set on EventResult when turn.completed carried counts.
	Usage Usage

	// Error is set on EventError. Prefer errors.As against *AuthError,
	// *RateLimitError, *ExitError, or *RunError.
	Error error
}

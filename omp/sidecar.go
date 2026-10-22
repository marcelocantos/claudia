// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// Verbs the broker sends. The sidecar streams Event messages back.
const (
	OpLoad    = "load"
	OpAdopt   = "adopt"
	OpPrompt  = "prompt"
	OpSteer   = "steer"
	OpAbort   = "abort"
	// OpPause / OpResume engage the sidecar process-global agentPauseGate
	// (🎯T966). In-flight streams and tools may finish; new model/tool work
	// parks until resume.
	OpPause  = "pause"
	OpResume = "resume"
	OpDrop   = "drop"
	OpReceipt = "receipt"
	OpTool    = "tool_result"
	// OpToken sets a plan's access token for every seat on it (🎯T159).
	// It carries Provider and Token and names no seat.
	OpToken     = "token"
	OpAuthRetry = "auth_retry"
)

// Message is one IPC line. Token is set only on load, and only with the
// access token the broker just read from the Keychain item.
type Message struct {
	Cursor    string `json:"cursor,omitempty"`
	Op        string `json:"op"`
	RequestID string `json:"request_id,omitempty"`
	RPCID     string `json:"rpc_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	After     uint64 `json:"after,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Seat      string `json:"seat,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Token     string `json:"token,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Text      string `json:"text,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Result    string `json:"result,omitempty"`
	// Turn fields name who prompted the seat (🎯T870). The sidecar
	// writes them onto the digest when it accepts the prompt.
	TurnID        string `json:"turn_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	RequireResume bool   `json:"require_resume,omitempty"`
	Cause         string `json:"cause,omitempty"`
	CauseDetail   string `json:"cause_detail,omitempty"`
	Resume        string `json:"resume,omitempty"`
	// Preserve and Pins steer the seat's compaction (claudia 🎯T152): what
	// its summary must keep, and facts carried verbatim after it.
	Preserve string   `json:"preserve,omitempty"`
	Pins     []string `json:"pins,omitempty"`
	// Tools is the host's own tool list (name, description, input_schema)
	// offered to a work seat on load and adopt, so the model can choose
	// them. Calls still come back to the host as tool_call (🎯T886).
	Tools json.RawMessage `json:"tools,omitempty"`
	// Sandbox limits the seat's own tools (claudia 🎯T178): Codex sandbox
	// settings mapped onto what the sidecar controls. Nil leaves them as
	// they are. A sidecar that honours it names the mode on ready.
	Sandbox *Sandbox `json:"sandbox,omitempty"`
	// ProtectedPaths are host edit rules (🎯T209 / jevons 🎯T546), independent of sandbox mode.
	ProtectedPaths []ProtectedPathRule `json:"protected_paths,omitempty"`
}

// Sandbox modes the sidecar enforces on its own tools.
const (
	SandboxReadOnly       = "read-only"
	SandboxWorkspaceWrite = "workspace-write"
)

// Sandbox is a seat's tool policy. read-only offers no Write and no Bash;
// workspace-write confines Write and Bash's file writes to the working
// directory plus WritableRoots, and keeps Protected read-only.
type ProtectedPathRule struct {
	Pattern string `json:"pattern"`
	Reason  string `json:"reason"`
}

// Sandbox is a seat's tool policy. read-only offers no Write and no Bash;
// workspace-write confines Write and Bash's file writes to the working
// directory plus WritableRoots, and keeps Protected read-only.
// ProtectedPaths are host rules (🎯T209 / jevons 🎯T546): mutating tools
// refuse matching paths with the given reason.
type Sandbox struct {
	Mode           string              `json:"mode"`
	WritableRoots  []string            `json:"writable_roots,omitempty"`
	Protected      []string            `json:"protected,omitempty"`
	ProtectedPaths []ProtectedPathRule `json:"protected_paths,omitempty"`
}

// Event is one sidecar line. Type turn_end carries a context snapshot.
type Event struct {
	GenerationPage *GenerationPage `json:"generation_page,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	RPCID          string          `json:"rpc_id,omitempty"`
	Receipt        *RequestReceipt `json:"receipt,omitempty"`
	UsageFact      *UsageFact      `json:"usage_fact,omitempty"`
	UsagePage      *UsagePage      `json:"usage_page,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	FailedToken    string          `json:"failed_token,omitempty"`
	Seat           string          `json:"seat,omitempty"`
	Type           string          `json:"type"`
	How            string          `json:"how,omitempty"`
	// Restored, on a "ready" for a launched seat, says whether the sidecar
	// brought back the seat's stored conversation (claudia 🎯T151, jevons
	// 🎯T929). Absent from a sidecar that predates the store, which never did.
	Restored *bool `json:"restored,omitempty"`
	// TranscriptEndsMidTurn, on a launched seat that restored, says whether
	// the saved transcript ended inside a turn (jevons 🎯T214). Absent from
	// a sidecar that predates it, or when the backend cannot say.
	TranscriptEndsMidTurn *bool `json:"transcript_ends_mid_turn,omitempty"`
	// ResumeChecked acknowledges required-resume admission, including adoption
	// of a matching in-memory generation. Old sidecars omit this field.
	ResumeChecked bool `json:"resume_checked,omitempty"`
	// BoundTools is the actual model-visible tool list after the sidecar
	// bound the offered tools. Nil means the sidecar did not attest binding.
	BoundTools []string `json:"bound_tools"`
	// Sandbox, on ready, is the tool policy mode the sidecar applied
	// (claudia 🎯T178). Empty from a sidecar that predates it.
	Sandbox string `json:"sandbox,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// StopReason, on a turn_end, says how the turn ended when that is not
	// its normal end: "aborted" for a turn the host cancelled. Empty is a
	// normal end. Never carried in Text, which is the turn's reply.
	StopReason string `json:"stop_reason,omitempty"`
	Text       string `json:"text,omitempty"`
	CallID     string `json:"call_id,omitempty"`
	Name       string `json:"name,omitempty"`
	// Error is the provider's refusal on a turn_end that got no answer
	// (usage limit, rate limit, auth). Empty on a turn that answered.
	Error    string          `json:"error,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
}

// Refusal is the provider's reason when a turn_end closed a refused turn,
// and "" otherwise (🎯T137). It reads Error, and falls back to the
// snapshot's last message for a sidecar started before Error existed: the
// sidecar outlives broker and host bounces, so an older one may be the one
// answering. An aborted turn is not a refusal.
func (ev Event) Refusal() string {
	if ev.Type != "turn_end" {
		return ""
	}
	if ev.Error != "" {
		return ev.Error
	}
	if len(ev.Snapshot) == 0 {
		return ""
	}
	var state struct {
		Messages []struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"messages"`
	}
	if json.Unmarshal(ev.Snapshot, &state) != nil || len(state.Messages) == 0 {
		return ""
	}
	last := state.Messages[len(state.Messages)-1]
	if last.Role != "assistant" || last.StopReason != "error" {
		return ""
	}
	if reason := strings.TrimSpace(last.ErrorMessage); reason != "" {
		return reason
	}
	return "the provider ended the turn with an error"
}

// Conn is one line-oriented connection to the long-lived sidecar.
type Conn struct {
	c net.Conn
	r *bufio.Reader
	w *bufio.Writer
}

// Dial dials the sidecar unix socket.
func Dial(ctx context.Context, path string) (*Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("omp: sidecar %s: %w", path, err)
	}
	return &Conn{c: c, r: bufio.NewReader(c), w: bufio.NewWriter(c)}, nil
}

func (c *Conn) Send(msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return c.w.Flush()
}

// SendContext bounds a write on a connection whose caller serializes writes.
// It does not change the read deadline or close a model's event stream.
func (c *Conn) SendContext(ctx context.Context, msg Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.c.SetWriteDeadline(deadline); err != nil {
			return err
		}
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.c.SetWriteDeadline(time.Now()); close(interrupted) })
	err := c.Send(msg)
	if !stop() {
		<-interrupted
	}
	resetErr := c.c.SetWriteDeadline(time.Time{})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return resetErr
}

func (c *Conn) Recv() (Event, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return Event{}, err
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// SetDeadline bounds the next read and write. The zero time clears it.
// Adopt uses it so a wedged sidecar cannot hold ResumeAll open.
func (c *Conn) SetDeadline(t time.Time) error {
	if c == nil || c.c == nil {
		return nil
	}
	return c.c.SetDeadline(t)
}

func (c *Conn) Close() error {
	if c == nil || c.c == nil {
		return nil
	}
	return c.c.Close()
}

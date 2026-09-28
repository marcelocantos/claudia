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
	OpLoad   = "load"
	OpAdopt  = "adopt"
	OpPrompt = "prompt"
	OpSteer  = "steer"
	OpAbort  = "abort"
	OpDrop   = "drop"
	OpTool   = "tool_result"
)

// Message is one IPC line. Token is set only on load, and only with the
// access token the broker just read from the Keychain item.
type Message struct {
	Op          string `json:"op"`
	Seat        string `json:"seat,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	SummaryOnly bool   `json:"summary_only,omitempty"`
	Token       string `json:"token,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Text        string `json:"text,omitempty"`
	CallID      string `json:"call_id,omitempty"`
	Result      string `json:"result,omitempty"`
	// Turn fields name who prompted the seat (🎯T870). The sidecar
	// writes them onto the digest when it accepts the prompt.
	TurnID      string `json:"turn_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Cause       string `json:"cause,omitempty"`
	CauseDetail string `json:"cause_detail,omitempty"`
	Resume      string `json:"resume,omitempty"`
	// Tools is the host's own tool list (name, description, input_schema)
	// offered to a work seat on load and adopt, so the model can choose
	// them. Calls still come back to the host as tool_call (🎯T886).
	Tools json.RawMessage `json:"tools,omitempty"`
}

// Event is one sidecar line. Type turn_end carries a context snapshot.
type Event struct {
	Seat   string `json:"seat,omitempty"`
	Type   string `json:"type"`
	How    string `json:"how,omitempty"`
	Reason string `json:"reason,omitempty"`
	Text   string `json:"text,omitempty"`
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
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
// Adopt uses it so an older sidecar that does not answer "adopt" cannot
// hold ResumeAll open.
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

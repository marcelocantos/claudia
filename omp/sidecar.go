// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
)

// Verbs the broker sends. The sidecar streams Event messages back.
const (
	OpLoad   = "load"
	OpPrompt = "prompt"
	OpSteer  = "steer"
	OpAbort  = "abort"
	OpTool   = "tool_result"
)

// Message is one IPC line. Token is set only on load, and only with the
// access token the broker just read from the Keychain item.
type Message struct {
	Op       string `json:"op"`
	Seat     string `json:"seat,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Token    string `json:"token,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Text     string `json:"text,omitempty"`
	CallID   string `json:"call_id,omitempty"`
	Result   string `json:"result,omitempty"`
	// Turn fields name who prompted the seat (🎯T870). The sidecar
	// writes them onto the digest when it accepts the prompt.
	TurnID      string `json:"turn_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Cause       string `json:"cause,omitempty"`
	CauseDetail string `json:"cause_detail,omitempty"`
	Resume      string `json:"resume,omitempty"`
}

// Event is one sidecar line. Type turn_end carries a context snapshot.
type Event struct {
	Seat     string          `json:"seat,omitempty"`
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	CallID   string          `json:"call_id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
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

func (c *Conn) Close() error {
	if c == nil || c.c == nil {
		return nil
	}
	return c.c.Close()
}

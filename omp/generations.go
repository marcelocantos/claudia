// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	GenerationPageLimit     = 128
	GenerationCursorLimit   = 96
	GenerationIdentityLimit = 1024
	GenerationIssueLimit    = 512
	DiscoveryCursorExpired  = "discovery_cursor_expired"
	DiscoveryBusy           = "discovery_busy"
)

// GenerationDiscoveryError is a typed discovery refusal. An expired cursor
// requires an empty-cursor restart and identity deduplication, not a skipped page.
type GenerationDiscoveryError struct{ Code, Message string }

func (e *GenerationDiscoveryError) Error() string { return e.Code + ": " + e.Message }

type GenerationIdentity struct {
	Seat      string `json:"seat"`
	SessionID string `json:"session_id"`
}
type GenerationIssue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GenerationPage is a weakly consistent directory traversal, not a filesystem
// snapshot or host ownership assertion. Complete means terminal traversal with
// no observed errors across its pages; concurrent creation requires later sweeps.
// Issues are page-local; Complete remembers prior-page issues. Empty pages may
// still have More: the limit also bounds examined non-history entries.
type GenerationPage struct {
	Generations []GenerationIdentity `json:"generations"`
	Issues      []GenerationIssue    `json:"issues"`
	Cursor      string               `json:"cursor"`
	More        bool                 `json:"more"`
	Complete    bool                 `json:"complete"`
}

func (p *GenerationPage) Validate(cursor string, limit int) error {
	if p == nil || limit < 1 || limit > GenerationPageLimit || len(cursor) > GenerationCursorLimit || p.Generations == nil || p.Issues == nil || len(p.Generations)+len(p.Issues) > limit || len(p.Cursor) > GenerationCursorLimit || p.More != (p.Cursor != "") || (p.More && (p.Cursor == cursor || p.Complete)) || (p.Complete && len(p.Issues) > 0) {
		return fmt.Errorf("invalid generation discovery page")
	}
	for _, g := range p.Generations {
		if strings.TrimSpace(g.Seat) == "" || strings.TrimSpace(g.SessionID) == "" || len(g.Seat) > GenerationIdentityLimit || len(g.SessionID) > GenerationIdentityLimit {
			return fmt.Errorf("invalid generation identity")
		}
	}
	for _, i := range p.Issues {
		if i.Code == "" || len(i.Code) > GenerationIssueLimit || len(i.Path) > GenerationIssueLimit || len(i.Message) > GenerationIssueLimit {
			return fmt.Errorf("invalid generation issue")
		}
	}
	return nil
}

// DiscoverGenerations reads history headers without loading agents or credentials.
func DiscoverGenerations(ctx context.Context, cursor string, limit int) (*GenerationPage, error) {
	if len(cursor) > GenerationCursorLimit || limit < 1 || limit > GenerationPageLimit {
		return nil, fmt.Errorf("invalid generation discovery query")
	}
	path, err := SocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := Dial(ctx, path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	rpc := uuid.NewString()
	msg := Message{Op: "generations", RPCID: rpc, Cursor: cursor, Limit: limit}
	if deadline, ok := ctx.Deadline(); ok {
		msg.ExpiresAt = deadline.UnixMilli()
	}
	if err := conn.Send(msg); err != nil {
		return nil, err
	}
	for {
		ev, err := conn.Recv()
		if err != nil {
			return nil, err
		}
		if ev.RPCID != rpc {
			continue
		}
		if ev.Type != "generation_page" {
			if ev.Reason == DiscoveryCursorExpired || ev.Reason == DiscoveryBusy {
				return nil, &GenerationDiscoveryError{Code: ev.Reason, Message: ev.Text}
			}
			return nil, fmt.Errorf("generation discovery: %s: %s", ev.Type, ev.Text)
		}
		if err := ev.GenerationPage.Validate(cursor, limit); err != nil {
			return nil, err
		}
		return ev.GenerationPage, nil
	}
}

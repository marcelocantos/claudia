// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ReceiptState is a fact recorded for one logical request, not a transport ACK.
type ReceiptState string

const (
	ReceiptAccepted  ReceiptState = "accepted"
	ReceiptStarted   ReceiptState = "started"
	ReceiptCompleted ReceiptState = "completed"
	ReceiptCancelled ReceiptState = "cancelled"
	ReceiptFailed    ReceiptState = "failed"
	ReceiptUnknown   ReceiptState = "unknown"
)

// RequestReceipt identifies an obligation within one durable seat generation.
// Unknown (including not_found) is uncertainty, never permission to replay.
type RequestReceipt struct {
	RequestID  string       `json:"request_id"`
	SessionID  string       `json:"session_id"`
	Seat       string       `json:"seat"`
	State      ReceiptState `json:"state"`
	AttemptID  string       `json:"attempt_id"`
	TurnIDs    []string     `json:"turn_ids"`
	Reason     string       `json:"reason,omitempty"`
	AcceptedAt string       `json:"accepted_at,omitempty"`
	UpdatedAt  string       `json:"updated_at,omitempty"`
}

// QueryReceipt reads the existing sidecar's journal without loading a seat or
// obtaining credentials. A disconnected or older sidecar is an error, not
// evidence that the request was never accepted.
func QueryReceipt(ctx context.Context, seat, session, request string) (*RequestReceipt, error) {
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
	if err := conn.Send(Message{Op: OpReceipt, Seat: seat, SessionID: session, RequestID: request, RPCID: rpc}); err != nil {
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
		if ev.Type == "error" {
			return nil, fmt.Errorf("receipt query: %s", ev.Text)
		}
		if ev.Type != "receipt" {
			return nil, fmt.Errorf("receipt query: unexpected response %q", ev.Type)
		}
		if err := ev.Receipt.ValidateIdentity(seat, session, request); err != nil {
			return nil, err
		}
		return ev.Receipt, nil
	}
}

// ValidateIdentity refuses incomplete or foreign receipts before a caller can
// use them as evidence. An old attempt is valid on a duplicate logical send.
func (r *RequestReceipt) ValidateIdentity(seat, session, request string) error {
	if r == nil || r.Seat != seat || r.SessionID != session || r.RequestID != request {
		return fmt.Errorf("receipt identity mismatch for seat %q session %q request %q", seat, session, request)
	}
	switch r.State {
	case ReceiptUnknown:
		return nil
	case ReceiptAccepted, ReceiptStarted, ReceiptCompleted, ReceiptCancelled, ReceiptFailed:
	default:
		return fmt.Errorf("invalid receipt state %q", r.State)
	}
	accepted, aerr := time.Parse(time.RFC3339Nano, r.AcceptedAt)
	updated, uerr := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	if strings.TrimSpace(r.AttemptID) == "" || aerr != nil || uerr != nil || updated.Before(accepted) || r.TurnIDs == nil {
		return fmt.Errorf("incomplete durable receipt for request %q", request)
	}
	seen := map[string]bool{}
	for _, id := range r.TurnIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return fmt.Errorf("invalid receipt turn identity")
		}
		seen[id] = true
	}
	if (r.State == ReceiptAccepted) != (len(r.TurnIDs) == 0) {
		return fmt.Errorf("receipt state and turn identities disagree")
	}
	return nil
}

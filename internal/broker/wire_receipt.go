// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import "strings"

const (
	TypeReceipt       MessageType = "receipt"
	TypeReceiptResult MessageType = "receipt_result"
)

// ReceiptRequest reads one durable obligation, without requiring a live grant.
type ReceiptRequest struct {
	Seat      string `json:"seat"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
}

func (r *ReceiptRequest) Validate() error {
	for _, field := range []struct{ name, value string }{{"seat", r.Seat}, {"session_id", r.SessionID}, {"request_id", r.RequestID}} {
		if strings.TrimSpace(field.value) == "" {
			return &ProtocolError{Code: CodeMissingField, Field: field.name, Msg: field.name + " is required"}
		}
	}
	return nil
}

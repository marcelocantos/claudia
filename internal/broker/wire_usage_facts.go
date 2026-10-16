// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"strings"

	"github.com/marcelocantos/claudia/omp"
)

const (
	TypeUsageFacts       MessageType = "usage_facts"
	TypeUsageFactsResult MessageType = "usage_facts_result"
)

type UsageFactsRequest struct {
	Seat      string `json:"seat"`
	SessionID string `json:"session_id"`
	After     uint64 `json:"after"`
	Limit     int    `json:"limit"`
	// Absolute caller deadline in Unix milliseconds. Zero is the legacy
	// omitted value; the direct query still imposes the server maximum.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

func (r *UsageFactsRequest) Validate() error {
	if strings.TrimSpace(r.Seat) == "" || strings.TrimSpace(r.SessionID) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "seat/session_id", Msg: "usage query requires seat and session"}
	}
	if r.Limit < 1 || r.Limit > omp.UsagePageLimit {
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "limit", Msg: "usage page limit must be bounded"}
	}
	if r.ExpiresAt < 0 {
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "expires_at", Msg: "usage query expiry must be nonnegative"}
	}
	return nil
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import "github.com/marcelocantos/claudia/omp"

const (
	TypeGenerations       MessageType = "generations"
	TypeGenerationsResult MessageType = "generations_result"
)

type GenerationsRequest struct {
	Cursor    string `json:"cursor"`
	Limit     int    `json:"limit"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

func (r *GenerationsRequest) Validate() error {
	if len(r.Cursor) > omp.GenerationCursorLimit || r.Limit < 1 || r.Limit > omp.GenerationPageLimit || r.ExpiresAt < 0 {
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "generations", Msg: "discovery requires bounded cursor, limit, and expiry"}
	}
	return nil
}

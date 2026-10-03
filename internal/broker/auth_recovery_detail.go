// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import "fmt"

// Separate operations preserve strict decoding of the legacy auth_recover
// acknowledgement. The request uses NamedRequest: no login permission exists.
const (
	TypeAuthRecoverDetail  MessageType = "auth_recover_detail"
	TypeAuthRecoveryDetail MessageType = "auth_recovery_detail"
)

type AuthRecoveryDetailResponse struct {
	Provider       string `json:"provider"`
	Outcome        string `json:"outcome"`
	Classification string `json:"classification"`
}

func (r *AuthRecoveryDetailResponse) Validate() error {
	if r.Provider == "" {
		return fmt.Errorf("detailed recovery provider missing")
	}
	switch r.Outcome {
	case "healthy_no_op", "refreshed":
		if r.Classification == "none" {
			return nil
		}
	case "failure":
		switch r.Classification {
		case "unsupported_provider", "store_unavailable", "needs_sign_in", "refresh_failed", "persistence_failed", "cancelled", "busy":
			return nil
		}
	}
	return fmt.Errorf("invalid detailed recovery outcome/classification")
}

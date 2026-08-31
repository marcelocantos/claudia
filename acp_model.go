// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"strings"
)

// acpSetModel switches the live ACP session model via the stable
// session/set_config_option path, falling back to legacy session/set_model
// for older agents (🎯T54).
func acpSetModel(request func(method string, params any) (json.RawMessage, error), sessionID, model string) error {
	sessionID = strings.TrimSpace(sessionID)
	model = strings.TrimSpace(model)
	if sessionID == "" {
		return fmt.Errorf("acp set model: empty session id")
	}
	if model == "" {
		return fmt.Errorf("acp set model: empty model")
	}

	// Prefer ACP session config options (category "model", configId "model").
	_, err := request("session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  "model",
		"value":     model,
	})
	if err == nil {
		return nil
	}
	cfgErr := err

	// Typed value shape used by some ACP v2 agents.
	_, err = request("session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  "model",
		"value":     map[string]any{"type": "id", "value": model},
	})
	if err == nil {
		return nil
	}
	typedErr := err

	// Legacy unstable method removed from recent ACP but still useful fallback.
	_, err = request("session/set_model", map[string]any{
		"sessionId": sessionID,
		"modelId":   model,
	})
	if err == nil {
		return nil
	}
	return fmt.Errorf("acp model switch failed: set_config_option(%v); typed(%v); set_model(%w)", cfgErr, typedErr, err)
}

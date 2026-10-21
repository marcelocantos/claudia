// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ConsumerIntentRelPath is the fleet-intent file under a Jevons state
// directory (internal/fleetintent.StoreFileName in the consumer repo).
const ConsumerIntentRelPath = "fleet/intent.json"

type consumerIntentDoc struct {
	Agents map[string]consumerIntentAgent `json:"agents"`
}

type consumerIntentAgent struct {
	State string `json:"state"`
}

// StoodDownFromConsumerIntent returns seat names the consumer's durable
// fleet intent records as deliberately not running (parked or reaped).
// A missing file is an empty set. The JSON shape matches Jevons
// fleet/intent.json; only agent rows are read.
func StoodDownFromConsumerIntent(path string) (map[string]bool, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]bool{}, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc consumerIntentDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]bool, len(doc.Agents))
	for name, agent := range doc.Agents {
		if consumerIntentStoodDown(agent.State) {
			out[name] = true
		}
	}
	return out, nil
}

func consumerIntentStoodDown(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "parked", "reaped":
		return true
	default:
		return false
	}
}

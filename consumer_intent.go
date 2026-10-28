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
//
// 🎯T233: the file is one union the consumer writes: genuine parked/reaped
// agent intent AND transient policy-held auto seats (e.g.
// frontier_consume.disabled) recorded with the same "parked" state. The
// broker does not distinguish the two — both mean "a consumer currently
// says do not run this" — and both are read fresh at every enforcement
// point rather than cached, so the consumer lifting a hold by rewriting
// the file is what un-holds a name; nothing on the broker side can.
const ConsumerIntentRelPath = "fleet/intent.json"

// ConsumerIntentMaxBytes bounds the file this process will read, so a
// runaway or corrupt writer cannot make the broker hold an unbounded read
// open on every boot and every grant.
const ConsumerIntentMaxBytes = 1 << 20 // 1 MiB

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
//
// Deprecated for any caller that must distinguish "no file" from "nothing
// held": that caller wants ConsumerIntentSnapshot instead (🎯T233.2).
func StoodDownFromConsumerIntent(path string) (map[string]bool, error) {
	stood, _, err := ConsumerIntentSnapshot(path)
	return stood, err
}

// ConsumerIntentSnapshot reads the consumer's current fleet intent file.
// present reports whether a valid snapshot was found: false for a missing
// or unparseable file. A caller enforcing a hold (🎯T233.2) MUST fail
// closed when present is false rather than treat the returned (nil) set
// as "nothing held" — those are different facts a cached-empty-map return
// cannot distinguish. An unconfigured path ("" — no consumer state dir)
// is reported present with an empty set: there is nothing to enforce.
func ConsumerIntentSnapshot(path string) (stood map[string]bool, present bool, err error) {
	if strings.TrimSpace(path) == "" {
		return map[string]bool{}, true, nil
	}
	info, statErr := os.Stat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil, false, nil
	}
	if statErr != nil {
		return nil, false, statErr
	}
	if info.Size() > ConsumerIntentMaxBytes {
		return nil, false, fmt.Errorf("%s: %d bytes exceeds %d byte limit", path, info.Size(), ConsumerIntentMaxBytes)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc consumerIntentDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]bool, len(doc.Agents))
	for name, agent := range doc.Agents {
		if consumerIntentStoodDown(agent.State) {
			out[name] = true
		}
	}
	return out, true, nil
}

func consumerIntentStoodDown(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "parked", "reaped":
		return true
	default:
		return false
	}
}

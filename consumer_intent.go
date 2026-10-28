// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConsumerIntentRelPath is the fleet-intent file under a Jevons state
// directory (internal/fleetintent.StoreFileName in the consumer repo).
//
// 🎯T233.2 (final owner schema): this file records ONLY genuine
// parked/reaped agent intent — an agent the consumer has actually stood
// down, not one it is transiently policy-holding. Dynamically policy-held
// auto seats (frontier_consume.disabled, target_achieved) are recorded
// separately in fleet/policy-hold.json (see policy_hold.go); the broker
// enforces the UNION of both files' held names, but the two files
// themselves are not a union — mixing them in one file was an earlier,
// superseded design. Both are read fresh at every enforcement point
// rather than cached, so the consumer lifting a hold by rewriting its
// file is what un-holds a name; nothing on the broker side can.
const ConsumerIntentRelPath = "fleet/intent.json"

// ConsumerIntentMaxBytes bounds the file this process will read, so a
// runaway or corrupt writer cannot make the broker hold an unbounded read
// open on every boot and every grant.
const ConsumerIntentMaxBytes = 1 << 20 // 1 MiB

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

// ConsumerIntentSnapshot reads the consumer's current fleet intent file
// (genuine parked/reaped agent intent only — 🎯T233.2 final schema).
// present reports whether a valid snapshot was found: false for a missing
// or unparseable file, or one whose "agents" key is absent or null. A
// caller enforcing a hold MUST fail closed when present is false rather
// than treat the returned (nil) set as "nothing held" — those are
// different facts a cached-empty-map return cannot distinguish. An
// unconfigured path ("" — no consumer state dir) is reported present
// with an empty set: there is nothing to enforce.
func ConsumerIntentSnapshot(path string) (stood map[string]bool, present bool, err error) {
	if strings.TrimSpace(path) == "" {
		return map[string]bool{}, true, nil
	}
	raw, present, err := readBoundedStateFile(path, ConsumerIntentMaxBytes)
	if err != nil || !present {
		return nil, false, err
	}
	// Decode into raw fields first so a document with no "agents" key, or
	// an explicit "agents": null, is told apart from a genuinely empty
	// {"agents": {}} — a writer that produced {} (missing the expected
	// top-level shape) must not be treated the same as an intentional
	// empty snapshot, or every held name would be silently revived.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	agentsRaw, hasAgents := fields["agents"]
	if !hasAgents || isJSONNull(agentsRaw) {
		return nil, false, fmt.Errorf("%s: missing or null %q key", path, "agents")
	}
	var agents map[string]consumerIntentAgent
	if err := json.Unmarshal(agentsRaw, &agents); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]bool, len(agents))
	for name, agent := range agents {
		if consumerIntentStoodDown(agent.State) {
			out[name] = true
		}
	}
	return out, true, nil
}

// isJSONNull reports whether a raw JSON value is the literal null,
// allowing for surrounding whitespace.
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func consumerIntentStoodDown(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "parked", "reaped":
		return true
	default:
		return false
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// openConsumerIntentFileNoFollow (unix: O_NOFOLLOW|O_NONBLOCK) is the
	// one syscall that both decides and acts: O_NOFOLLOW fails the open
	// with ELOOP on a symlink instead of traversing it, and O_NONBLOCK
	// means opening a FIFO with no writer attached returns immediately
	// rather than blocking this goroutine forever — there is no separate
	// Lstat-then-Open TOCTOU window to close here because there is no
	// separate Lstat; the single open call is both the check and the use
	// (🎯T233.2 follow-up). The trust boundary this still rests on is the
	// consumer state dir being private to its writer and this broker, not
	// an attacker-writable path; that is a deployment property, not
	// something this function can enforce.
	f, err := openConsumerIntentFileNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// The fd's own Stat is what gets size- and type-checked, not the
	// path's: whatever the open actually attached to is what gets read.
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s: refusing non-regular file (mode %v)", path, info.Mode())
	}
	if info.Size() > ConsumerIntentMaxBytes {
		return nil, false, fmt.Errorf("%s: %d bytes exceeds %d byte limit", path, info.Size(), ConsumerIntentMaxBytes)
	}
	// Bounded read regardless of what Stat claimed: a file that grows
	// between stat and read (e.g. a writer appending in place rather than
	// atomically renaming) is cut off at the limit rather than read
	// without bound.
	raw, err := io.ReadAll(io.LimitReader(f, ConsumerIntentMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > ConsumerIntentMaxBytes {
		return nil, false, fmt.Errorf("%s: exceeds %d byte limit while reading", path, ConsumerIntentMaxBytes)
	}
	// Decode into raw fields first so a document with no "agents" key, or
	// an explicit "agents": null, is told apart from a genuinely empty
	// {"agents": {}} — the PO finding this closes (🎯T233.2 follow-up): a
	// writer that produced {} (missing the expected top-level shape) was
	// treated the same as an intentional empty snapshot and silently
	// revived every held name. Only an explicit, present, non-null
	// "agents" object counts as a valid (possibly empty) snapshot; a
	// missing or null "agents" key is reported invalid so the caller
	// fails closed exactly as it would for a missing file.
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

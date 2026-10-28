// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PolicyHoldRelPath is the Jevons-owned policy-hold file under a consumer
// state directory (🎯T233.2 final owner schema). It records ONLY
// transient policy-held auto seats — a hold the daemon's own dynamic
// policy is imposing right now (frontier_consume.disabled, or the
// target the auto was working is already achieved) — never a genuine
// parked/reaped agent intent; that is fleet/intent.json's job
// (consumer_intent.go) and the two files are not interchangeable. The
// broker enforces the union of both files' held names.
const PolicyHoldRelPath = "fleet/policy-hold.json"

// PolicyHoldMaxBytes bounds the file this process will read.
const PolicyHoldMaxBytes = 1 << 20 // 1 MiB

// PolicyHoldMaxEntries bounds how many hold entries one snapshot may
// name, so a runaway writer cannot make the broker build an unbounded
// map on every boot and every grant.
const PolicyHoldMaxEntries = 4096

// PolicyHoldVersion is the only schema version this reader accepts.
const PolicyHoldVersion = 1

// PolicyHoldKind is the required "kind" discriminator.
const PolicyHoldKind = "policy_hold"

// Policy-hold reasons the writer may record. 🎯T233.2 final owner
// schema (strict): these are the ONLY two reasons PolicyHoldSnapshot
// accepts — any other value (including empty) fails the whole snapshot
// closed, it is not silently dropped or passed through. A new reason
// genuinely needs a broker release before it is valid.
const (
	PolicyHoldReasonFrontierConsumeDisabled = "frontier_consume_disabled"
	PolicyHoldReasonTargetAchieved          = "target_achieved"
)

// PolicyHoldKnownReason reports whether reason is one of the two
// documented policy-hold reasons. PolicyHoldSnapshot uses this itself to
// reject any entry whose reason is not in this set.
func PolicyHoldKnownReason(reason string) bool {
	switch reason {
	case PolicyHoldReasonFrontierConsumeDisabled, PolicyHoldReasonTargetAchieved:
		return true
	default:
		return false
	}
}

// policyHoldDoc is the strict v1 shape:
//
//	{"version":1,"kind":"policy_hold","updated_at":"RFC3339","holds":[{"name":"jv-t1020-auto","reason":"frontier_consume_disabled"}]}
type policyHoldDoc struct {
	Version   json.RawMessage  `json:"version"`
	Kind      json.RawMessage  `json:"kind"`
	UpdatedAt json.RawMessage  `json:"updated_at"`
	Holds     *json.RawMessage `json:"holds"`
}

type policyHoldEntry struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// PolicyHoldSnapshot reads the consumer's current policy-hold file.
// held maps name to reason. present reports whether a valid v1
// policy_hold snapshot was found: false for a missing file, invalid
// JSON, a wrong/missing version or kind, a missing/null/malformed
// "holds" list, an oversize file, more than PolicyHoldMaxEntries
// entries, an entry with an empty name, an entry with an empty or
// unrecognised reason (only frontier_consume_disabled and
// target_achieved are accepted — 🎯T233.2 strict final schema), or a
// repeated name anywhere in "holds" (the snapshot must be a complete,
// internally-unique document; it is rejected whole, not narrowed, on
// any of these). A caller enforcing a hold MUST fail closed when
// present is false. An unconfigured path ("" — no consumer state dir)
// is reported present with an empty map: there is nothing to enforce.
func PolicyHoldSnapshot(path string) (held map[string]string, present bool, err error) {
	if strings.TrimSpace(path) == "" {
		return map[string]string{}, true, nil
	}
	raw, present, err := readBoundedStateFile(path, PolicyHoldMaxBytes)
	if err != nil || !present {
		return nil, false, err
	}
	var doc policyHoldDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if err := requireJSONNumberEquals(doc.Version, "version", PolicyHoldVersion); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if err := requireJSONStringEquals(doc.Kind, "kind", PolicyHoldKind); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if isJSONAbsentOrNull(doc.UpdatedAt) {
		return nil, false, fmt.Errorf("%s: missing or null %q key", path, "updated_at")
	}
	var updatedAt string
	if err := json.Unmarshal(doc.UpdatedAt, &updatedAt); err != nil {
		return nil, false, fmt.Errorf("%s: %q: %w", path, "updated_at", err)
	}
	if _, err := time.Parse(time.RFC3339, updatedAt); err != nil {
		return nil, false, fmt.Errorf("%s: %q is not RFC3339: %w", path, "updated_at", err)
	}
	if doc.Holds == nil || isJSONAbsentOrNull(*doc.Holds) {
		return nil, false, fmt.Errorf("%s: missing or null %q key", path, "holds")
	}
	var entries []policyHoldEntry
	if err := json.Unmarshal(*doc.Holds, &entries); err != nil {
		return nil, false, fmt.Errorf("%s: %q: %w", path, "holds", err)
	}
	if len(entries) > PolicyHoldMaxEntries {
		return nil, false, fmt.Errorf("%s: %d holds exceeds %d entry limit", path, len(entries), PolicyHoldMaxEntries)
	}
	// Strict v1 (PO finding, release-blocking): a snapshot is REJECTED
	// outright — not silently narrowed — if any entry has an empty or
	// unrecognised reason, or if any name repeats. "Complete, unique
	// snapshot" means every entry is individually valid and the whole
	// document is internally consistent; a writer bug that produces two
	// conflicting rows for one name, or a reason outside the documented
	// set, is exactly the kind of silent-wrong-interpretation case this
	// reader exists to refuse rather than paper over with last-wins or
	// best-effort reads.
	out := make(map[string]string, len(entries))
	for i, e := range entries {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return nil, false, fmt.Errorf("%s: holds[%d]: empty name", path, i)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return nil, false, fmt.Errorf("%s: holds[%d] (%q): empty reason", path, i, name)
		}
		if !PolicyHoldKnownReason(e.Reason) {
			return nil, false, fmt.Errorf("%s: holds[%d] (%q): reason %q is not %s or %s", path, i, name, e.Reason,
				PolicyHoldReasonFrontierConsumeDisabled, PolicyHoldReasonTargetAchieved)
		}
		if _, dup := out[name]; dup {
			return nil, false, fmt.Errorf("%s: holds[%d]: duplicate name %q", path, i, name)
		}
		out[name] = e.Reason
	}
	return out, true, nil
}

// requireJSONNumberEquals unmarshals raw as a JSON number and requires it
// to equal want; raw being absent, null, or any other shape is an error.
func requireJSONNumberEquals(raw json.RawMessage, field string, want int) error {
	if isJSONAbsentOrNull(raw) {
		return fmt.Errorf("missing or null %q key", field)
	}
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("%q: %w", field, err)
	}
	if got != want {
		return fmt.Errorf("%q: %d, want %d", field, got, want)
	}
	return nil
}

// requireJSONStringEquals unmarshals raw as a JSON string and requires it
// to equal want; raw being absent, null, or any other shape is an error.
func requireJSONStringEquals(raw json.RawMessage, field string, want string) error {
	if isJSONAbsentOrNull(raw) {
		return fmt.Errorf("missing or null %q key", field)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("%q: %w", field, err)
	}
	if got != want {
		return fmt.Errorf("%q: %q, want %q", field, got, want)
	}
	return nil
}

// isJSONAbsentOrNull reports whether raw is unset (nil, for a missing
// key — encoding/json leaves a RawMessage field nil rather than writing
// the literal "null" bytes into it) or holds the JSON literal null.
func isJSONAbsentOrNull(raw json.RawMessage) bool {
	return raw == nil || isJSONNull(raw)
}

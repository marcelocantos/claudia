// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePolicyHoldFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 🎯T233.2 final owner schema: a valid v1 policy_hold snapshot is read,
// and only its holds are reported.
func TestPolicyHoldSnapshotValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	writePolicyHoldFile(t, path, `{
  "version": 1,
  "kind": "policy_hold",
  "updated_at": "2026-10-28T12:00:00Z",
  "holds": [
    {"name": "jv-t1020-auto", "reason": "frontier_consume_disabled"},
    {"name": "jv-t1030-auto", "reason": "target_achieved"}
  ]
}`)
	held, present, err := PolicyHoldSnapshot(path)
	if err != nil {
		t.Fatalf("valid snapshot: %v", err)
	}
	if !present {
		t.Fatal("valid snapshot reported not present")
	}
	if held["jv-t1020-auto"] != "frontier_consume_disabled" || held["jv-t1030-auto"] != "target_achieved" {
		t.Fatalf("held = %v", held)
	}
	if len(held) != 2 {
		t.Fatalf("held = %v, want exactly 2 entries", held)
	}
}

// 🎯T233.2: an explicit, present, non-null empty holds list is a valid,
// intentional "nothing held" snapshot.
func TestPolicyHoldSnapshotValidEmptyHolds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	writePolicyHoldFile(t, path, `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[]}`)
	held, present, err := PolicyHoldSnapshot(path)
	if err != nil {
		t.Fatalf("empty holds: %v", err)
	}
	if !present {
		t.Fatal("empty holds reported not present")
	}
	if len(held) != 0 {
		t.Fatalf("held = %v, want empty", held)
	}
}

// 🎯T233.2: a missing file is not present (fail-closed signal for the
// caller), not an empty held set.
func TestPolicyHoldSnapshotMissingFileIsNotPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	held, present, err := PolicyHoldSnapshot(path)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if present {
		t.Fatal("missing file reported present")
	}
	if held != nil {
		t.Fatalf("missing file: held = %v, want nil", held)
	}
}

// 🎯T233.2: an unconfigured path is present with an empty map — nothing
// to enforce, distinct from a configured-but-unreadable file.
func TestPolicyHoldSnapshotUnconfiguredPathIsPresentEmpty(t *testing.T) {
	held, present, err := PolicyHoldSnapshot("")
	if err != nil {
		t.Fatalf("unconfigured path: %v", err)
	}
	if !present {
		t.Fatal("unconfigured path reported not present")
	}
	if len(held) != 0 {
		t.Fatalf("unconfigured path: held = %v, want empty", held)
	}
}

// 🎯T233.2: strict schema rejects every malformed shape rather than
// silently reading a partial/garbled document as "nothing held".
func TestPolicyHoldSnapshotStrictSchemaRejections(t *testing.T) {
	valid := func(extra string) string {
		return fmt.Sprintf(`{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[%s]}`, extra)
	}
	cases := map[string]string{
		"bare object":            `{}`,
		"wrong version":          `{"version":2,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[]}`,
		"missing version":        `{"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[]}`,
		"wrong kind":             `{"version":1,"kind":"fleet_intent","updated_at":"2026-10-28T12:00:00Z","holds":[]}`,
		"missing kind":           `{"version":1,"updated_at":"2026-10-28T12:00:00Z","holds":[]}`,
		"missing updated_at":     `{"version":1,"kind":"policy_hold","holds":[]}`,
		"null updated_at":        `{"version":1,"kind":"policy_hold","updated_at":null,"holds":[]}`,
		"non-RFC3339 updated_at": `{"version":1,"kind":"policy_hold","updated_at":"not-a-date","holds":[]}`,
		"missing holds":          `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z"}`,
		"null holds":             `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":null}`,
		"holds not a list":       `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":{}}`,
		"entry with empty name":  valid(`{"name":"","reason":"target_achieved"}`),
		"not even JSON":          `not json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, PolicyHoldRelPath)
			writePolicyHoldFile(t, path, body)
			held, present, err := PolicyHoldSnapshot(path)
			if err == nil {
				t.Fatalf("%s: want error", name)
			}
			if present {
				t.Fatalf("%s: reported present", name)
			}
			if held != nil {
				t.Fatalf("%s: held = %v, want nil", name, held)
			}
		})
	}
}

// 🎯T233.2 strict final schema (PO release-blocking finding): a reason
// outside the documented set (frontier_consume_disabled |
// target_achieved) REJECTS the whole snapshot — it is not read through
// or silently dropped. PolicyHoldKnownReason itself still reports the
// known/unknown distinction, used internally by PolicyHoldSnapshot to
// decide the rejection.
func TestPolicyHoldSnapshotUnknownReasonRejectsWholeSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	writePolicyHoldFile(t, path, `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[{"name":"jv-x","reason":"something_new"}]}`)
	held, present, err := PolicyHoldSnapshot(path)
	if err == nil {
		t.Fatal("unknown reason: want error")
	}
	if present {
		t.Fatal("unknown reason reported present")
	}
	if held != nil {
		t.Fatalf("unknown reason: held = %v, want nil", held)
	}
	if !PolicyHoldKnownReason(PolicyHoldReasonFrontierConsumeDisabled) || !PolicyHoldKnownReason(PolicyHoldReasonTargetAchieved) {
		t.Fatal("documented reasons not reported known")
	}
	if PolicyHoldKnownReason("something_new") {
		t.Fatal("something_new reported as a known reason")
	}
}

// 🎯T233.2 strict final schema (PO release-blocking finding): an empty
// reason string REJECTS the whole snapshot, same as an unrecognised one.
func TestPolicyHoldSnapshotEmptyReasonRejectsWholeSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	writePolicyHoldFile(t, path, `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[{"name":"jv-x","reason":""}]}`)
	held, present, err := PolicyHoldSnapshot(path)
	if err == nil {
		t.Fatal("empty reason: want error")
	}
	if present {
		t.Fatal("empty reason reported present")
	}
	if held != nil {
		t.Fatalf("empty reason: held = %v, want nil", held)
	}
}

// 🎯T233.2 strict final schema (PO release-blocking finding): a
// repeated name anywhere in "holds" REJECTS the whole snapshot — not
// last-wins, not first-wins. A writer producing two conflicting rows
// for the same name is a writer bug, and this is exactly the class of
// silent-wrong-interpretation case the strict schema exists to refuse.
func TestPolicyHoldSnapshotDuplicateNameRejectsWholeSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	writePolicyHoldFile(t, path, `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[
		{"name":"jv-x","reason":"frontier_consume_disabled"},
		{"name":"jv-x","reason":"target_achieved"}
	]}`)
	held, present, err := PolicyHoldSnapshot(path)
	if err == nil {
		t.Fatal("duplicate name: want error")
	}
	if present {
		t.Fatal("duplicate name reported present")
	}
	if held != nil {
		t.Fatalf("duplicate name: held = %v, want nil", held)
	}
}

// 🎯T233.2: a duplicate name with the identical reason twice is still
// rejected — "complete, unique snapshot" means no name repeats at all,
// not merely no conflicting reasons for the same name.
func TestPolicyHoldSnapshotDuplicateNameSameReasonStillRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	writePolicyHoldFile(t, path, `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[
		{"name":"jv-x","reason":"frontier_consume_disabled"},
		{"name":"jv-x","reason":"frontier_consume_disabled"}
	]}`)
	held, present, err := PolicyHoldSnapshot(path)
	if err == nil {
		t.Fatal("duplicate name (same reason): want error")
	}
	if present {
		t.Fatal("duplicate name (same reason) reported present")
	}
	if held != nil {
		t.Fatalf("duplicate name (same reason): held = %v, want nil", held)
	}
}

// 🎯T233.2: more than PolicyHoldMaxEntries holds is refused rather than
// building an unbounded map.
func TestPolicyHoldSnapshotTooManyEntriesRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	var entries []string
	for i := 0; i <= PolicyHoldMaxEntries; i++ {
		entries = append(entries, fmt.Sprintf(`{"name":"jv-%d","reason":"target_achieved"}`, i))
	}
	body := fmt.Sprintf(`{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[%s]}`, strings.Join(entries, ","))
	writePolicyHoldFile(t, path, body)
	held, present, err := PolicyHoldSnapshot(path)
	if err == nil {
		t.Fatal("too many entries: want error")
	}
	if present {
		t.Fatal("too many entries reported present")
	}
	if held != nil {
		t.Fatalf("too many entries: held = %v, want nil", held)
	}
}

// 🎯T233.2: an oversize file is refused rather than read.
func TestPolicyHoldSnapshotOversizeFileRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyHoldRelPath)
	big := make([]byte, PolicyHoldMaxBytes+1)
	for i := range big {
		big[i] = ' '
	}
	writePolicyHoldFile(t, path, string(big))
	held, present, err := PolicyHoldSnapshot(path)
	if err == nil {
		t.Fatal("oversize: want error")
	}
	if present {
		t.Fatal("oversize reported present")
	}
	if held != nil {
		t.Fatalf("oversize: held = %v, want nil", held)
	}
}

// 🎯T233.2: a symlink at the policy-hold path is refused, reusing the
// same safe-open path as the intent-file reader.
func TestPolicyHoldSnapshotSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-policy-hold.json")
	writePolicyHoldFile(t, real, `{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[]}`)
	path := filepath.Join(dir, PolicyHoldRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	held, present, err := PolicyHoldSnapshot(path)
	if err == nil {
		t.Fatal("symlink: want error")
	}
	if present {
		t.Fatal("symlink reported present")
	}
	if held != nil {
		t.Fatalf("symlink: held = %v, want nil", held)
	}
}

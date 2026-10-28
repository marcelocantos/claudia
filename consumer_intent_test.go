// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoodDownFromConsumerIntent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{
  "fleet": {"state": "working"},
  "agents": {
    "jv-parked": {"state": "parked"},
    "jv-reaped": {"state": "reaped"},
    "jv-live": {"state": "working"},
    "jv-owner": {"state": "blocked_owner"}
  }
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := StoodDownFromConsumerIntent(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got["jv-parked"] || !got["jv-reaped"] || got["jv-live"] || got["jv-owner"] {
		t.Fatalf("stood down = %v", got)
	}
}

// 🎯T233.2: a missing file is reported not-present so a caller enforcing a
// hold can fail closed, distinct from a present-but-empty snapshot.
func TestConsumerIntentSnapshotMissingFileIsNotPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	stood, present, err := ConsumerIntentSnapshot(path)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if present {
		t.Fatal("missing file reported present")
	}
	if stood != nil {
		t.Fatalf("missing file: stood = %v, want nil", stood)
	}
}

// 🎯T233.2: invalid JSON is reported not-present with an error, not an
// empty held set.
func TestConsumerIntentSnapshotInvalidFileIsNotPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	stood, present, err := ConsumerIntentSnapshot(path)
	if err == nil {
		t.Fatal("invalid file: want error")
	}
	if present {
		t.Fatal("invalid file reported present")
	}
	if stood != nil {
		t.Fatalf("invalid file: stood = %v, want nil", stood)
	}
}

// 🎯T233.2: an unconfigured path (no consumer state dir) is present with
// an empty set — nothing to enforce, not a fail-closed condition.
func TestConsumerIntentSnapshotUnconfiguredPathIsPresentEmpty(t *testing.T) {
	stood, present, err := ConsumerIntentSnapshot("")
	if err != nil {
		t.Fatalf("unconfigured path: %v", err)
	}
	if !present {
		t.Fatal("unconfigured path reported not present")
	}
	if len(stood) != 0 {
		t.Fatalf("unconfigured path: stood = %v, want empty", stood)
	}
}

// 🎯T233.2: a valid file over the byte limit is refused rather than read.
func TestConsumerIntentSnapshotOversizeFileRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, ConsumerIntentMaxBytes+1)
	for i := range big {
		big[i] = ' '
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	_, present, err := ConsumerIntentSnapshot(path)
	if err == nil {
		t.Fatal("oversize file: want error")
	}
	if present {
		t.Fatal("oversize file reported present")
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"syscall"
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

// 🎯T233.2 PO follow-up: {} (no "agents" key at all) is a malformed/
// incomplete write, not a valid empty snapshot — it must fail closed the
// same as a missing file, not be read as "nothing held".
func TestConsumerIntentSnapshotBareObjectIsNotPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stood, present, err := ConsumerIntentSnapshot(path)
	if err == nil {
		t.Fatal("{} : want error (missing agents key)")
	}
	if present {
		t.Fatal("{} reported present")
	}
	if stood != nil {
		t.Fatalf("{} : stood = %v, want nil", stood)
	}
}

// 🎯T233.2 PO follow-up: {"agents": null} is likewise not present.
func TestConsumerIntentSnapshotNullAgentsIsNotPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"agents": null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stood, present, err := ConsumerIntentSnapshot(path)
	if err == nil {
		t.Fatal(`{"agents": null} : want error`)
	}
	if present {
		t.Fatal(`{"agents": null} reported present`)
	}
	if stood != nil {
		t.Fatalf(`{"agents": null} : stood = %v, want nil`, stood)
	}
}

// 🎯T233.2 PO follow-up: {"agents": {}} IS a valid, intentional empty
// snapshot (a writer that tracks no agents at all) and must stay present
// with an empty held set — the fix for the two cases above must not also
// break this one.
func TestConsumerIntentSnapshotExplicitEmptyAgentsIsPresentEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"agents": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stood, present, err := ConsumerIntentSnapshot(path)
	if err != nil {
		t.Fatalf(`{"agents": {}} : %v`, err)
	}
	if !present {
		t.Fatal(`{"agents": {}} reported not present`)
	}
	if len(stood) != 0 {
		t.Fatalf(`{"agents": {}} : stood = %v, want empty`, stood)
	}
}

// 🎯T233.2 PO follow-up: a symlink at the intent path is refused without
// being followed, even when it points at an otherwise valid snapshot.
func TestConsumerIntentSnapshotSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-intent.json")
	if err := os.WriteFile(real, []byte(`{"agents":{"jv-x":{"state":"parked"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	stood, present, err := ConsumerIntentSnapshot(path)
	if err == nil {
		t.Fatal("symlink: want error")
	}
	if present {
		t.Fatal("symlink reported present")
	}
	if stood != nil {
		t.Fatalf("symlink: stood = %v, want nil", stood)
	}
}

// 🎯T233.2 PO follow-up: a directory at the intent path is refused, not
// opened.
func TestConsumerIntentSnapshotDirectoryRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	stood, present, err := ConsumerIntentSnapshot(path)
	if err == nil {
		t.Fatal("directory: want error")
	}
	if present {
		t.Fatal("directory reported present")
	}
	if stood != nil {
		t.Fatalf("directory: stood = %v, want nil", stood)
	}
}

// 🎯T233.2 PO follow-up: a FIFO at the intent path is refused. The open
// itself (unix: O_NONBLOCK) does not block waiting for a writer — this
// call is expected to return promptly on its own, with no wall-clock
// deadline machinery in the test (🎯T97): a hang here fails the way any
// other hermetic test hang does, under `go test -timeout`.
func TestConsumerIntentSnapshotFIFORefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConsumerIntentRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo not supported on this platform: %v", err)
	}
	stood, present, err := ConsumerIntentSnapshot(path)
	if err == nil {
		t.Fatal("fifo: want error")
	}
	if present {
		t.Fatal("fifo reported present")
	}
	if stood != nil {
		t.Fatalf("fifo: stood = %v, want nil", stood)
	}
}

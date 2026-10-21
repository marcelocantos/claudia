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

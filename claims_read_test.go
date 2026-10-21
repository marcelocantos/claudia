// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoodDownSeatsFromClaims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ClaimsFile)
	if got, err := StoodDownSeats(path); err != nil || len(got) != 0 {
		t.Fatalf("missing file = %v %v, want empty", got, err)
	}
	raw := `{
  "version": 1,
  "seats": {
    "parked": {"stood_down": "2026-10-21T12:00:00Z"},
    "live": {"held": "2026-10-21T13:00:00Z"}
  }
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := StoodDownSeats(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got["parked"] || got["live"] {
		t.Fatalf("stood down = %v", got)
	}
}

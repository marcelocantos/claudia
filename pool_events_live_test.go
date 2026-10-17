// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// TestPoolAgentEventsLive keeps its live-gate name. The CLI pool entry
// points are deleted, so the body asserts Release refuses.
func TestPoolAgentEventsLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test starts a real Claude session)")
	}
	for _, bin := range []string{"claude", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	if err := (&Agent{}).Release("return"); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Release = %v, want legacy refusal", err)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// TestPoolAgentEventsLive keeps its live-gate name. The direct pool
// refuses a CLI seat, so the body asserts that refusal.
func TestPoolAgentEventsLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test starts a real Claude session)")
	}
	for _, bin := range []string{"claude", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	agent, err := AcquireDirect(context.Background(), Config{WorkDir: t.TempDir(), Model: "haiku"})
	if agent != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = (%v, %v), want legacy refusal", agent, err)
	}
}

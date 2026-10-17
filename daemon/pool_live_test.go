// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/marcelocantos/claudia"
)

// TestAcquireLive keeps its live-gate name. Acquire refuses a CLI pool
// seat, so the body asserts that refusal and does not start a daemon.
func TestAcquireLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test starts a real Claude session)")
	}
	for _, bin := range []string{"claude", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	agent, err := claudia.Acquire(context.Background(), claudia.Config{WorkDir: t.TempDir(), Model: "haiku"})
	if agent != nil || !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = (%v, %v), want legacy refusal", agent, err)
	}
}

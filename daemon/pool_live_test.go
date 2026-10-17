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

// TestAcquireLive keeps its live-gate name. The daemon's acquire path
// refuses without launching a pool seat.
func TestAcquireLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test starts a real Claude session)")
	}
	for _, bin := range []string{"claude", "tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	agent, err := daemonAcquire(context.Background(), claudia.Config{WorkDir: t.TempDir(), Model: "haiku"})
	if agent != nil || !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("daemonAcquire = (%v, %v), want legacy refusal", agent, err)
	}
}

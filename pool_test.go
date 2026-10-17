// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// skipIfNoBinaries skips real-pool integration tests unless CLAUDIA_LIVE=1
// and claude+tmux are on PATH. The names stay on the live gate. With the
// env set, the bodies assert the CLI refusal and return.
func skipIfNoBinaries(t *testing.T) {
	t.Helper()
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (pool Acquire starts a real Claude session)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

func assertAcquireRefuses(t *testing.T) {
	t.Helper()
	agent, err := Acquire(context.Background(), Config{WorkDir: t.TempDir()})
	if agent != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = (%v, %v), want legacy refusal", agent, err)
	}
}

// TestPoolKeyConsistency checks that poolKeyFor is stable and
// deterministic (no randomness, no dependency on env).
func TestPoolKeyConsistency(t *testing.T) {
	k1 := poolKeyFor("/workdir", "sonnet", "Tool1")
	k2 := poolKeyFor("/workdir", "sonnet", "Tool1")
	k3 := poolKeyFor("/workdir", "opus", "Tool1")
	k4 := poolKeyFor("/other", "sonnet", "Tool1")

	if k1 != k2 {
		t.Errorf("poolKeyFor is not deterministic: %q vs %q", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("different models produce same key")
	}
	if k1 == k4 {
		t.Errorf("different workdirs produce same key")
	}
	if len(k1) != 12 {
		t.Errorf("key length = %d, want 12 hex chars", len(k1))
	}
}

// TestPoolWindowNaming verifies the window-name scheme so that
// ListWindows matching works as expected.
func TestPoolWindowNaming(t *testing.T) {
	key := poolKeyFor("/tmp/testdir", "haiku", "Agent,SendMessage,EnterWorktree")
	name := poolWindowPrefix + key
	if !strings.HasPrefix(name, "claudia-pool-") {
		t.Errorf("pool window name %q does not start with claudia-pool-", name)
	}
}

// TestAcquireColdAndReturn keeps its live-gate name. The CLI pool no
// longer mints a window, so the body asserts the refusal.
func TestAcquireColdAndReturn(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestAcquireDropKillsWindow keeps its live-gate name and asserts the refusal.
func TestAcquireDropKillsWindow(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestAcquireKeepAliveFor keeps its live-gate name and asserts the refusal.
func TestAcquireKeepAliveFor(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestAcquireConcurrentDifferentKeys keeps its live-gate name and asserts the refusal.
func TestAcquireConcurrentDifferentKeys(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestAcquireHeldWindowNotReused keeps its live-gate name and asserts the refusal.
func TestAcquireHeldWindowNotReused(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestAcquirePoolCapEviction keeps its live-gate name and asserts the refusal.
func TestAcquirePoolCapEviction(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestAcquireErrorPolicy keeps its live-gate name and asserts the refusal.
func TestAcquireErrorPolicy(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestPoolCrashSurvival keeps its live-gate name. It does not spawn the
// helper, and the helper does not block.
func TestPoolCrashSurvival(t *testing.T) {
	skipIfNoBinaries(t)
	assertAcquireRefuses(t)
}

// TestPoolCrashSurvivalHelper is the child side of TestPoolCrashSurvival.
// A direct run with the helper env asserts the refusal and returns.
func TestPoolCrashSurvivalHelper(t *testing.T) {
	if os.Getenv("CLAUDIA_POOL_CRASH_HELPER") != "1" {
		t.Skip("helper — only runs as subprocess of TestPoolCrashSurvival")
	}
	workDir := os.Getenv("CLAUDIA_POOL_CRASH_WORKDIR")
	if workDir == "" {
		workDir = t.TempDir()
	}
	agent, err := Acquire(context.Background(), Config{WorkDir: workDir})
	if agent != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = (%v, %v), want legacy refusal", agent, err)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// skipIfNoBinaries skips unless CLAUDIA_LIVE=1 and claude+tmux are on PATH.
// The names stay on the live gate. With the env set, the bodies assert that
// a local seat's Release refuses. Acquire and AcquireDirect are deleted.
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

func assertPoolReleaseRefuses(t *testing.T) {
	t.Helper()
	err := (&Agent{}).Release("return")
	if !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Release = %v, want legacy refusal", err)
	}
}

func TestAcquireColdAndReturn(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestAcquireDropKillsWindow(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestAcquireKeepAliveFor(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestAcquireConcurrentDifferentKeys(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestAcquireHeldWindowNotReused(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestAcquirePoolCapEviction(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestAcquireErrorPolicy(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestPoolCrashSurvival(t *testing.T) {
	skipIfNoBinaries(t)
	assertPoolReleaseRefuses(t)
}

func TestPoolCrashSurvivalHelper(t *testing.T) {
	if os.Getenv("CLAUDIA_POOL_CRASH_HELPER") != "1" {
		t.Skip("helper — only runs as subprocess of TestPoolCrashSurvival")
	}
	assertPoolReleaseRefuses(t)
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// TestMain keeps the hermetic suite off any claudia daemon installed on the
// machine. Start, Task.Run and LoadPlanUsage consult the socket when one
// exists (🎯T3); without this a developer's live daemon would be granted
// every fixture seat the suite starts, with real provider processes behind
// them. Tests that want a daemon start their own on a temp socket and
// re-enable the consult with t.Setenv(broker.NoBrokerEnv, "").
func TestMain(m *testing.M) {
	if os.Getenv(broker.NoBrokerEnv) == "" {
		_ = os.Setenv(broker.NoBrokerEnv, "1")
	}
	// Fixtures publish rate-limit errors, and a direct agent that sees one
	// marks the default plan-usage cache stale (🎯T75.4). Keep that off the
	// developer's real cache.
	cache, err := os.MkdirTemp("", "claudia-plan-cache")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CLAUDIA_PLAN_CACHE", cache)
	// Every tmux seat the suite starts goes on a server of its own, so the
	// fleet's pane census cannot reap a test window out from under a
	// running assertion (🎯T77). See planTestTmuxSocket.
	stopTmux := usePrivateTmuxServer()
	code := m.Run()
	stopTmux()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}

// waitFor polls cond until it holds or the test's own -timeout is near.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	backstop := wallclockguard.UntilTestTimeout(t)
	for backstop.Err() == nil {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

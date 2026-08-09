// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/testctlenv"
)

// TestNoTestControlEnvLeak is the standing guard for target T20: no
// test-control handshake variable may be present in the environment
// this suite starts in.
//
// The variables it checks are private handshakes between a parent test
// and the child process it spawns (TestCrashSurvival ->
// TestCrashSurvivalHelper, TestPoolCrashSurvival ->
// TestPoolCrashSurvivalHelper). Outside that handshake they have no
// legitimate source, so their presence means an ambient environment
// silently changed what this suite does. When it happened for real,
// CLAUDIA_CRASH_TEST_HELPER=1 was frozen into claudia's shared tmux
// server by a helper subprocess that outlived its parent, and every
// agent window created afterwards inherited it — `go test ./...` in
// those windows un-skipped the helper and hung until the 600s panic
// timeout.
//
// The hang is the loud version. The dangerous version is the inverse:
// a leaked variable that makes a test SKIP inside a fleet agent while
// it passes on the owner's machine, so the agent reports green for a
// test that never ran. This test fails fast and names the variable so
// neither outcome is mistaken for a repo defect.
//
// It runs on the normal `go test ./...` path — including CI, which
// invokes that command bare — so the guard cannot be forgotten.
//
// Deliberately not checked here are the live gates (CLAUDIA_LIVE and
// friends). A human may set those on purpose, and their presence is
// indistinguishable from a deliberate choice. They are still stripped
// from every spawned agent environment; see package testctlenv and
// TestEnsureServerStripsTestControlEnv.
func TestNoTestControlEnvLeak(t *testing.T) {
	leaked := testctlenv.LeakedHelpers(os.LookupEnv)
	if len(leaked) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("test-control variables leaked into this process's environment:\n")
	for _, name := range leaked {
		b.WriteString("\t" + name + "=" + os.Getenv(name) + "\n")
	}
	b.WriteString("\nThese are private handshakes between a parent test and its own\n")
	b.WriteString("child subprocess; nothing else may set them. While they are set,\n")
	b.WriteString("this suite does not run what the owner's suite runs.\n\n")
	b.WriteString("Most likely source: claudia's tmux server froze them into its\n")
	b.WriteString("global environment, and this shell is an agent window that\n")
	b.WriteString("inherited it. Check with:\n")
	b.WriteString("\ttmux -S \"${CLAUDIA_TMUX_SOCKET:-$HOME/.local/state/claudia/tmux.sock}\" show-environment -g\n")
	b.WriteString("Current claudia scrubs these on tmuxagent.EnsureServer; a window\n")
	b.WriteString("opened before that guard landed keeps the stale environment until\n")
	b.WriteString("it is replaced.\n")
	t.Fatal(b.String())
}

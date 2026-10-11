// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"slices"
	"testing"
)

func disallowedIn(t *testing.T, argv []string) string {
	t.Helper()
	i := slices.Index(argv, "--disallowedTools")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv carries no --disallowedTools: %v", argv)
	}
	return argv[i+1]
}

// TestTaskAlwaysDisallowsAgentSpawning is the guarantee the package
// documents and Task mode did not keep.
//
// A task that can spawn sub-agents takes process-lifecycle ownership away
// from the host, and turns any imperative sentence in its input into a
// fan-out. That is not hypothetical: a summariser handed a transcript
// containing "go deep with fanout" obeyed it and produced roughly 33,000
// subagents and 4.3 billion tokens in two hours.
func TestSessionAndTaskShareTheBaseline(t *testing.T) {
	if got := disallowedToolList(nil); got != BaseDisallowedTools {
		t.Errorf("disallowedToolList(nil) = %q, want the shared baseline %q",
			got, BaseDisallowedTools)
	}
}

// TestCodexTaskToolRestrictionsFailClosed (🎯T4.6) is the same failure
// mode as the tests above, one provider over. `codex exec` has no
// per-tool disallow flag, so honouring DisallowTools is impossible —
// and running anyway would hand the caller a fully-armed agent while
// they believe shell and filesystem access were removed. The run must
// be refused with a typed error before any process is spawned.
//
// This test is hermetic on purpose: no CODEX_BIN, no auth. If the guard
// is removed the run reaches the auth preflight and fails with a
// different, untyped error, so the assertion is on the type, not merely
// on failure.

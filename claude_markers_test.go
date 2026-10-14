// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"slices"
	"testing"
)

func TestStripClaudeSessionMarkersKeepsConfiguration(t *testing.T) {
	t.Parallel()
	env := []string{
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_SESSION_ID=abc",
		"CLAUDECODE=1",
		"CLAUDE_EFFORT=high",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"PATH=/bin",
	}
	got := stripClaudeSessionMarkers(env)
	want := []string{"CLAUDE_EFFORT=high", "CLAUDE_CODE_USE_BEDROCK=1", "PATH=/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

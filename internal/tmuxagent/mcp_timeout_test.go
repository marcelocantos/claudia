// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A seat launched on a loaded host kept losing MCP servers to Claude Code's
// 30 s attach bound (🎯T123). Every window is given a longer one, and a host
// that exports its own keeps it.
func TestNewWindowCarriesAnMCPTimeout(t *testing.T) {
	unset := func(string) string { return "" }
	if got, want := mcpTimeoutEnv(unset), "MCP_TIMEOUT=180000"; got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}
	host := func(k string) string {
		if k == MCPTimeoutEnvVar {
			return " 60000 "
		}
		return ""
	}
	if got, want := mcpTimeoutEnv(host), "MCP_TIMEOUT=60000"; got != want {
		t.Fatalf("host value = %q, want %q", got, want)
	}
}

// The same, through a real tmux server on a private socket: the variable has
// to be in the seat's own environment, not merely in an argument list.
func TestSpawnedWindowSeesTheMCPTimeout(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env.txt")
	id, err := SpawnWindow(t.TempDir(), "mcp-timeout-probe", "sh",
		[]string{"-c", "printenv MCP_TIMEOUT > " + out})
	if id != "" || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("SpawnWindow wrote the seat environment: %v", statErr)
	}
}

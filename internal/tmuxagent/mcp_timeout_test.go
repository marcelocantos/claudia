// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// A unix socket path is limited to about 104 bytes, and t.TempDir() under
	// /var/folders with this test's name in it is longer than that.
	sockDir, err := os.MkdirTemp("/tmp", "cl-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "t.sock")
	t.Setenv(tmuxSocketEnvVar, sock)
	t.Setenv(MCPTimeoutEnvVar, "") // the host exports nothing: the default applies
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })

	// The window writes its environment, then signals a tmux channel. The test
	// blocks on that channel: the event is the wait, and `go test -timeout` is
	// the only clock (🎯T97).
	out := filepath.Join(t.TempDir(), "env.txt")
	const channel = "mcp-timeout-probe-done"
	id, err := SpawnWindow(t.TempDir(), "mcp-timeout-probe", "sh",
		[]string{"-c", "printenv MCP_TIMEOUT > " + out + "; tmux -S " + sock + " wait-for -S " + channel})
	if err != nil {
		t.Fatalf("SpawnWindow: %v", err)
	}
	t.Cleanup(func() { _ = KillWindow(id) })

	if b, err := exec.Command("tmux", "-S", sock, "wait-for", channel).CombinedOutput(); err != nil {
		t.Fatalf("waiting for the window's signal: %v: %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the window signalled but wrote no environment: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != DefaultSeatMCPTimeoutMS {
		t.Fatalf("seat sees MCP_TIMEOUT=%q, want %q", got, DefaultSeatMCPTimeoutMS)
	}
}

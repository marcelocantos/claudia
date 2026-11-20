// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// kill signals only the direct child. A server started through a wrapper
// that is wedged and not reading stdin must not survive close.
func TestMCPStdioKillReachesGrandchild(t *testing.T) {
	marker := fmt.Sprintf("mcp-wrapper-%d", os.Getpid())
	b := newMCPStdioBackend(MCPServer{Name: "wrapped", Command: "/bin/sh",
		Args: []string{"-c", "sleep 600; true # " + marker}})
	t.Cleanup(b.close)

	note := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(note, &msg); err != nil {
		t.Fatal(err)
	}
	if _, err := b.roundTrip(context.Background(), note, msg); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	shPID := b.proc.cmd.Process.Pid
	b.mu.Unlock()
	var child int
	waitFor(t, "wrapper to start its server", func() bool {
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(shPID)).Output()
		if err != nil {
			return false
		}
		n, conv := strconv.Atoi(strings.TrimSpace(string(out)))
		if conv != nil || n <= 1 {
			return false
		}
		child = n
		return true
	})
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	b.close()
	waitFor(t, "wrapper child to exit", func() bool { return syscall.Kill(child, 0) != nil })
}

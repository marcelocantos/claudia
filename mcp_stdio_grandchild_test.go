// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// kill signals only the direct child. A server started through a wrapper
// that is wedged and not reading stdin must not survive close.
func TestMCPStdioKillReachesGrandchild(t *testing.T) {
	marker := fmt.Sprintf("mcp-wrapper-%d", time.Now().UnixNano())
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
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && child == 0; time.Sleep(50 * time.Millisecond) {
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(shPID)).Output()
		child, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	if child == 0 {
		t.Fatal("wrapper never started its server")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	b.close()
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(child, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(child, 0); err == nil {
		t.Fatalf("server process %d (child of killed wrapper %d) is still running after close()", child, shPID)
	}
}

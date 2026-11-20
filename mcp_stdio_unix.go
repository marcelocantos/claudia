// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package claudia

import (
	"os/exec"
	"syscall"
)

// setMCPStdioProcessGroup puts a stdio server in its own process group.
// npx, uvx and a shell wrapper fork the real server; killing only the
// wrapper leaves that server running (M13).
func setMCPStdioProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killMCPStdioProcess ends the server and every process that stayed in
// its group. A descendant that left the group is outside this.
func killMCPStdioProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

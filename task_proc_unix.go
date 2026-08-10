// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package claudia

import (
	"context"
	"os/exec"
	"syscall"
)

// setTaskProcessGroup puts the task child in its own process group so Cancel
// and context cancel can signal the whole tree. Without this, killing only the
// shell leaves grandchildren (e.g. background sleep) holding StdoutPipe open
// and the event scanner hangs forever (🎯T17).
func setTaskProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// armTaskProcessGroupKill kills the task process group when ctx is done.
// CommandContext only signals the leader; grandchildren that inherited the
// stdout pipe must die too or Scan never sees EOF.
func armTaskProcessGroupKill(ctx context.Context, cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		<-ctx.Done()
		killTaskProcessGroup(cmd)
	}()
}

func interruptTaskProcess(cmd *exec.Cmd) error {
	return signalTaskProcessGroup(cmd, syscall.SIGINT)
}

func killTaskProcessGroup(cmd *exec.Cmd) {
	_ = signalTaskProcessGroup(cmd, syscall.SIGKILL)
}

func signalTaskProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Negative pid addresses the process group (requires Setpgid).
	return syscall.Kill(-cmd.Process.Pid, sig)
}

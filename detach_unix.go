// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package claudia

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the child in a new session so consumer exit/SIGHUP
// does not tear it down (connect-mode serve durability).
func detachProcess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package claudia

import "os/exec"

// setMCPStdioProcessGroup has no process-group equivalent here.
func setMCPStdioProcessGroup(cmd *exec.Cmd) {}

// killMCPStdioProcess ends the direct child. Windows has no group kill of
// the kind setMCPStdioProcessGroup uses.
func killMCPStdioProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package claudia

import (
	"os"
	"os/exec"
)

func processAliveImpl(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Windows FindProcess always succeeds; OpenProcess would be better
	// but zero-signal probe is not portable. Best-effort: try to open
	// via tasklist is too heavy — treat as alive only if we can signal.
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Windows Signal is not implemented for 0; assume alive if Find succeeds.
	_ = p
	return true
}

func detachProcess(cmd *exec.Cmd) {
	// No Setsid on Windows; CREATE_NEW_PROCESS_GROUP would be ideal.
	// Leave default — connect-mode primary target is macOS/Linux (jevons).
}

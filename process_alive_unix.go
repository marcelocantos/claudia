// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package claudia

import (
	"os"
	"syscall"
)

func processAliveImpl(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes existence without delivering a real signal.
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

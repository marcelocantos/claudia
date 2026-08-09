// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package claudia

import (
	"context"
	"os"
	"os/exec"
)

func setTaskProcessGroup(cmd *exec.Cmd) {
	// Windows process groups need CREATE_NEW_PROCESS_GROUP; task Cancel/Stop
	// hermetics are POSIX-only. Leader kill via CommandContext is enough here.
}

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
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}

func killTaskProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

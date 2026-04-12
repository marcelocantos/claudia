// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"fmt"
	"os/exec"
	"strings"
)

// IsWindowAlive checks whether the given window ID still exists on
// the claudia tmux server. A failed display-message means the
// window (or the entire server) is gone.
func IsWindowAlive(windowID string) bool {
	sock := SocketPath()
	return exec.Command(
		"tmux", "-S", sock,
		"display-message", "-p", "-t", windowID, "",
	).Run() == nil
}

// ResizeWindow changes the terminal dimensions of the given window.
func ResizeWindow(windowID string, cols, rows uint16) error {
	sock := SocketPath()
	out, err := exec.Command(
		"tmux", "-S", sock,
		"resize-window", "-t", windowID,
		"-x", fmt.Sprintf("%d", cols),
		"-y", fmt.Sprintf("%d", rows),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux resize-window: %w: %s", err, out)
	}
	return nil
}

// SetWindowOption stores a user-defined option on the window via
// tmux set-option -w. Used to persist the session_id for
// crash-survival recovery.
func SetWindowOption(windowID, key, value string) error {
	sock := SocketPath()
	out, err := exec.Command(
		"tmux", "-S", sock,
		"set-option", "-w", "-t", windowID,
		"@"+key, value,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux set-option %s: %w: %s", key, err, out)
	}
	return nil
}

// GetWindowOption reads a user-defined option from the window.
// Returns the value and true if set, or ("", false) if unset.
func GetWindowOption(windowID, key string) (string, bool) {
	sock := SocketPath()
	out, err := exec.Command(
		"tmux", "-S", sock,
		"show-options", "-wv", "-t", windowID,
		"@"+key,
	).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

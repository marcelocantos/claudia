// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"os/exec"
	"strings"
)

// IsWindowAlive checks whether the given window ID still exists on
// the claudia tmux server. It uses list-windows to enumerate all
// windows and checks for an exact ID match. This is more reliable than
// display-message, which falls back to the active window for bad
// targets and always exits 0.
func IsWindowAlive(windowID string) bool {
	sock := SocketPath()
	out, err := exec.Command(
		"tmux", "-S", sock,
		"list-windows", "-a",
		"-F", "#{window_id}",
	).Output()
	if err != nil {
		// Server not running or other error — window can't be alive.
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == windowID {
			return true
		}
	}
	return false
}

// ResizeWindow changes the terminal dimensions of the given window.
func ResizeWindow(windowID string, cols, rows uint16) error {
	return ErrLegacyCLISeatTransport
}

// SetWindowOption refuses. It used to store a user option via
// tmux set-option -w.
func SetWindowOption(windowID, key, value string) error {
	return ErrLegacyCLISeatTransport
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

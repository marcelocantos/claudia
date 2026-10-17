// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

// IsWindowAlive refuses. It used to list tmux windows. A seat no longer
// has one, so this reports the window absent before tmux runs.
func IsWindowAlive(windowID string) bool {
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

// GetWindowOption refuses. It used to read a tmux user option.
// An option that was not read is ("", false).
func GetWindowOption(windowID, key string) (string, bool) {
	return "", false
}

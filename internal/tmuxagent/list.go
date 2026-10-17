// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

// WindowInfo holds identifying information for a tmux window.
type WindowInfo struct {
	// ID is the tmux window ID, e.g. "@3".
	ID string
	// Name is the window name, e.g. "claudia-pool-abc123def456".
	Name string
}

// ListWindows used to return every window on the claudia tmux server.
// A Session seat no longer has that server. The signature stays; the
// error is returned before tmux runs.
func ListWindows() ([]WindowInfo, error) {
	return nil, ErrLegacyCLISeatTransport
}

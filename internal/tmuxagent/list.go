// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"fmt"
	"os/exec"
	"strings"
)

// WindowInfo holds identifying information for a tmux window.
type WindowInfo struct {
	// ID is the tmux window ID, e.g. "@3".
	ID string
	// Name is the window name, e.g. "claudia-pool-abc123def456".
	Name string
}

// ListWindows returns all windows on the claudia tmux server. Returns
// an empty slice (not an error) if the server is not running.
func ListWindows() ([]WindowInfo, error) {
	sock := SocketPath()

	out, err := exec.Command(
		"tmux", "-S", sock,
		"list-windows", "-a",
		"-F", "#{window_id} #{window_name}",
	).Output()
	if err != nil {
		// If the server is not running, list-windows exits non-zero.
		// Treat that as an empty list rather than an error.
		if ee, ok := err.(*exec.ExitError); ok {
			// Server not running: "no server running" or similar.
			msg := strings.ToLower(string(ee.Stderr) + string(out))
			if strings.Contains(msg, "no server") ||
				strings.Contains(msg, "can't connect") ||
				strings.Contains(msg, "error connecting") {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("tmux list-windows: %w", wrapExitErr(err))
	}

	var windows []WindowInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		windows = append(windows, WindowInfo{ID: strings.TrimSpace(id), Name: strings.TrimSpace(name)})
	}
	return windows, nil
}

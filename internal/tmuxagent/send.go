// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"fmt"
	"os/exec"
)

// SendKeys sends the given text to the target window literally (via
// send-keys -l), then submits with CR (Enter). Embedded newlines in
// msg become LF (Shift+Enter in Claude Code's TUI, inserting a
// newline without submitting), and the trailing CR submits the turn.
func SendKeys(windowID, msg string) error {
	sock := SocketPath()
	// Step 1: type the message literally.
	if msg != "" {
		if out, err := exec.Command(
			"tmux", "-S", sock,
			"send-keys", "-t", windowID, "-l", msg,
		).CombinedOutput(); err != nil {
			return fmt.Errorf("tmux send-keys (message): %w: %s", err, out)
		}
	}
	// Step 2: press Enter (CR) to submit the turn.
	if out, err := exec.Command(
		"tmux", "-S", sock,
		"send-keys", "-t", windowID, "-l", "\r",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys (submit): %w: %s", err, out)
	}
	return nil
}

// SendEscape sends the Escape key (0x1b) to the target window.
// Matches Agent.Interrupt's semantics of cancelling the current turn.
// Uses send-keys without -l so "Escape" is interpreted as the key
// name rather than typed literally.
func SendEscape(windowID string) error {
	sock := SocketPath()
	if out, err := exec.Command(
		"tmux", "-S", sock,
		"send-keys", "-t", windowID, "Escape",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys Escape: %w: %s", err, out)
	}
	return nil
}

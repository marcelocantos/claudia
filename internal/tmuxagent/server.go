// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package tmuxagent implements the tmux substrate for claudia's
// Agent. It spawns Claude Code inside a tmux window on a dedicated
// claudia tmux server and drives the session via send-keys,
// capture-pane, and control-mode notifications.
//
// The dedicated server runs on a socket under
// $XDG_STATE_HOME/claudia/tmux.sock (with ~/.local/state fallback),
// overridable via CLAUDIA_TMUX_SOCKET. It is separate from the user's
// default tmux so claudia windows never appear in the user's
// workspace and claudia's lifecycle is decoupled from the user's
// habits.
//
// This package is part of the T1.1 tmux-pivot milestone. See
// docs/targets.yaml for the broader pivot context.
package tmuxagent

import (
	"os"
	"path/filepath"
)

// tmuxSocketEnvVar overrides the default socket path when set.
const tmuxSocketEnvVar = "CLAUDIA_TMUX_SOCKET"

// anchorSessionName is the detached placeholder session held open on
// the claudia tmux server so the server stays alive even when all
// agent windows die. Claudia agent windows are created inside this
// session; kill-window on a real agent doesn't touch the anchor.
const anchorSessionName = "claudia-anchor"

// SocketPath returns the dedicated claudia tmux server socket path.
// Honours CLAUDIA_TMUX_SOCKET when set, otherwise falls back to
// $XDG_STATE_HOME/claudia/tmux.sock (with ~/.local/state as the
// XDG fallback).
func SocketPath() string {
	if p := os.Getenv(tmuxSocketEnvVar); p != "" {
		return p
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(stateHome, "claudia", "tmux.sock")
}

// EnsureServer used to start the dedicated claudia tmux server and scrub
// its global environment. A Session seat no longer has that server. The
// signature stays so existing callers still build; the error is returned
// before tmux runs, and an already-running server is left untouched.
func EnsureServer() error {
	return ErrLegacyCLISeatTransport
}

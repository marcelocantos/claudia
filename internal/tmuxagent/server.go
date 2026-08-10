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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/marcelocantos/claudia/internal/testctlenv"
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

var ensureMu sync.Mutex

// EnsureServer starts the dedicated claudia tmux server if it is not
// already running. Idempotent and safe to call concurrently from
// multiple goroutines in the same process.
//
// The server is brought up via
//
//	tmux -S <sock> new-session -d -s claudia-anchor
//
// which creates a detached session running the user's shell. That
// session is the placeholder anchor — agent windows are created
// inside it with `tmux new-window -t claudia-anchor:` and can be
// kill-windowed independently without taking the server down.
//
// The server is started with claudia's test-control variables stripped
// (see package testctlenv). A tmux server freezes the environment of
// whichever process started it as its global environment, and every
// window created afterwards inherits it — so a variable that happened
// to be set when the server came up would otherwise reach every agent
// for the life of the server. That is not hypothetical: a
// crash-survival helper subprocess, whose whole purpose is to outlive
// its parent, once left CLAUDIA_CRASH_TEST_HELPER=1 baked into the
// shared server, and `go test ./...` inside every later agent window
// hung on the un-skipped helper. See target T20.
//
// An already-running server is scrubbed rather than restarted, so a
// server poisoned before this guard existed heals on the next call.
func EnsureServer() error {
	ensureMu.Lock()
	defer ensureMu.Unlock()

	sock := SocketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return fmt.Errorf("create tmux socket dir: %w", err)
	}

	// `tmux -S <sock> list-sessions` exits 0 iff a server is running
	// on that socket. Any error means we need to start one.
	if err := exec.Command("tmux", "-S", sock, "list-sessions").Run(); err == nil {
		return scrubServerEnv(sock)
	}

	cmd := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", anchorSessionName)
	cmd.Env = testctlenv.Strip(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start tmux server: %w: %s", err, out)
	}
	return scrubServerEnv(sock)
}

// scrubServerEnv removes any test-control variable from the running
// server's global environment. Windows inherit the server's global
// environment, not the environment of the client that ran new-window,
// so this is the one place the leak can be closed for an existing
// server.
//
// The common case costs a single tmux call: show-environment is read
// first and unset is issued only for variables actually present.
func scrubServerEnv(sock string) error {
	out, err := exec.Command("tmux", "-S", sock, "show-environment", "-g").Output()
	if err != nil {
		return fmt.Errorf("tmux show-environment: %w", wrapExitErr(err))
	}

	present := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Removed variables are reported as "-NAME"; set ones as "NAME=value".
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "-"), "=")
		present[name] = true
	}

	for _, name := range testctlenv.All() {
		if !present[name] {
			continue
		}
		cmd := exec.Command("tmux", "-S", sock, "set-environment", "-g", "-u", name)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmux unset %s: %w: %s", name, err, out)
		}
	}
	return nil
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Where the broker lives, and whether to use it at all (🎯T2.1).
//
// The socket path is resolved here and nowhere else. Everything that needs it —
// the daemon that binds, the client that dials, the library deciding whether a
// broker is reachable — calls SocketPath, so "where is the broker?" has one
// answer rather than one per caller. A second spelling of the path is not a
// duplicated constant, it is a split brain: the daemon binds one socket and the
// client waits forever on another, and nothing reports an error.
//
// Both overrides exist for the same reason the brokerless fallback does. A test
// must be able to run a broker without touching the developer's real
// ~/.local/state/claudia, and a machine must be able to carry two brokers
// without them fighting over one path.

const (
	// SocketPathEnv names the socket outright, overriding the state-directory
	// computation entirely.
	SocketPathEnv = "CLAUDIA_BROKER_SOCKET"

	// NoBrokerEnv switches the broker off for the process that sets it, sending
	// the library down its brokerless path even when a broker is listening.
	NoBrokerEnv = "CLAUDIA_NO_BROKER"

	// StateHomeEnv is the XDG base-directory variable. ~/.local/state is its
	// specified fallback, so honouring it is not a second policy — it is the
	// same policy, asked of the environment before it is assumed.
	StateHomeEnv = "XDG_STATE_HOME"
)

const (
	// socketName is the socket's basename inside the state directory.
	socketName = "broker.sock"
	// stateSubdir is claudia's own directory inside the state home.
	stateSubdir = "claudia"
	// xdgLocalDir and xdgStateDir spell XDG's specified fallback for
	// $XDG_STATE_HOME, as path segments below the user's home directory.
	xdgLocalDir = ".local"
	xdgStateDir = "state"
	// stateDirPerm keeps the state directory owner-only. The socket inside it
	// grants whoever can connect the ability to start processes as this user,
	// so the directory mode is access control, not tidiness.
	stateDirPerm = 0o700
	// socketPerm restricts the socket itself, belt and braces with the
	// directory: a umask that admits the group would otherwise be enough to
	// hand out that same capability.
	socketPerm = 0o600
)

// negativeEnvValues are the spellings of "off" recognised in NoBrokerEnv, so
// that CLAUDIA_NO_BROKER=0 does the obvious thing rather than the literal one.
// Anything else non-empty disables the broker: setting the variable at all is
// overwhelmingly an intent to switch it off, and a typo must not silently leave
// it on.
var negativeEnvValues = map[string]bool{"": true, "0": true, "false": true, "no": true, "off": true}

// Disabled reports whether the broker is switched off for this process.
//
// It is deliberately decided before any socket is touched, so a caller that
// sets NoBrokerEnv gets the brokerless path with no filesystem or network
// syscall in between — which is what makes the fallback usable as an escape
// hatch when the broker itself is what is broken.
func Disabled() bool {
	return !negativeEnvValues[strings.ToLower(strings.TrimSpace(os.Getenv(NoBrokerEnv)))]
}

// StateDir returns claudia's state directory, honouring StateHomeEnv and
// falling back to ~/.local/state/claudia.
func StateDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv(StateHomeEnv)); home != "" {
		// XDG requires an absolute path here and says a relative one must be
		// ignored, rather than resolved against a working directory that
		// differs between the daemon and its clients.
		if filepath.IsAbs(home) {
			return filepath.Join(home, stateSubdir), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("broker: locate home directory for the state path: %w", err)
	}
	return filepath.Join(home, xdgLocalDir, xdgStateDir, stateSubdir), nil
}

// SocketPath returns the absolute path the broker listens on.
func SocketPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(SocketPathEnv)); override != "" {
		return absoluteSocketPath(override)
	}
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return absoluteSocketPath(filepath.Join(dir, socketName))
}

// absoluteSocketPath normalises and length-checks one candidate socket path.
//
// Absolute, because a relative AF_UNIX path resolves against each process's own
// working directory: the daemon and its clients would name the same socket and
// reach different ones. Length-checked, because the kernel's sun_path is a
// fixed array — an over-long path fails the bind with a bare EINVAL that says
// nothing about which of the two limits was hit.
func absoluteSocketPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("broker: resolve socket path %q: %w", path, err)
	}
	// The kernel needs room for a terminating NUL, so the usable length is one
	// short of the array — the same comparison Go's own syscall layer makes
	// before it hands the address to bind(2).
	if len(abs) >= maxSocketPathLen {
		return "", fmt.Errorf("broker: socket path %q is %d bytes; the platform limit is %d "+
			"(set %s to a shorter path)", abs, len(abs), maxSocketPathLen-1, SocketPathEnv)
	}
	return abs, nil
}

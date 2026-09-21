// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SpawnWindow creates a new agent window on the claudia tmux server
// running the given command with args in workdir. Returns the tmux
// window ID (e.g. "@3") on success.
//
// The command and its arguments are shell-quoted into a single
// string and passed to tmux new-window as one shell-command argument.
// tmux forwards the string to `sh -c`, which is the most
// cross-version-reliable way to launch a program with arguments
// under tmux: older tmuxes don't all accept argv-splitting after
// `--`, but every tmux in circulation accepts a single
// shell-command string.
//
// CLAUDECODE is explicitly cleared on the spawned window's
// environment so Claude Code does not detect itself as nested when
// the host Go program is itself running inside a Claude Code
// harness. This mirrors the strip in agent.go:196.
func SpawnWindow(workdir, windowName, command string, args []string) (windowID string, err error) {
	if err := EnsureServer(); err != nil {
		return "", err
	}
	sock := SocketPath()

	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("resolve workdir: %w", err)
	}
	// Resolve symlinks so project-dir escaping matches Claude Code's
	// canonicalisation. On macOS /var is a symlink to /private/var
	// and any workdir under /var/folders (including t.TempDir)
	// produces transcripts under the resolved path. Mirrors
	// agent.go:138-140.
	if resolved, err := filepath.EvalSymlinks(absWorkdir); err == nil {
		absWorkdir = resolved
	}

	shellCmd := shellJoin(append([]string{command}, args...))

	tmuxArgs := []string{
		"-S", sock,
		"new-window",
		"-d", // don't switch the attached client to the new window
		"-P", // print the new window's info on stdout
		"-F", "#{window_id}",
		"-c", absWorkdir,
		"-t", anchorSessionName + ":",
		"-n", windowName,
		"-e", "CLAUDECODE=",
		"-e", mcpTimeoutEnv(os.Getenv),
		shellCmd,
	}

	out, err := exec.Command("tmux", tmuxArgs...).Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}

	// 🎯T579: EnsureServer proved a server was up, but the window is
	// created by a second process a moment later, and in between the
	// server can go away — the anchor session is reaped, the last
	// window exits, or a kill-server lands. The failure the fleet sees
	// is "no server running on <sock>", and because the caller treats
	// it as a spawn failure the agent never comes back. The socket was
	// never wrong; the server was gone. Bring it back and try once
	// more. A second failure is reported as it stands.
	first := wrapExitErr(err)
	if !serverGone(first) {
		return "", fmt.Errorf("tmux new-window: %w", first)
	}
	if err := EnsureServer(); err != nil {
		return "", fmt.Errorf("tmux new-window: %w (restarting server: %v)", first, err)
	}
	out, err = exec.Command("tmux", tmuxArgs...).Output()
	if err != nil {
		return "", fmt.Errorf("tmux new-window: %w", wrapExitErr(err))
	}
	return strings.TrimSpace(string(out)), nil
}

// serverGone reports a tmux error that means the server on our socket
// was not there, rather than a bad argument or a missing target. tmux
// says "no server running on <path>" for a dead socket and "can't find
// session"/"session not found" when the server is up but the anchor
// session is not.
func serverGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no server running") ||
		strings.Contains(s, "can't find session") ||
		strings.Contains(s, "session not found") ||
		strings.Contains(s, "no such session")
}

// KillWindow kill-windows the given window ID. Idempotent: a missing
// window is treated as success, since the post-condition ("window
// doesn't exist") is already satisfied.
func KillWindow(windowID string) error {
	sock := SocketPath()
	cmd := exec.Command("tmux", "-S", sock, "kill-window", "-t", windowID)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	s := string(out)
	if strings.Contains(s, "can't find") || strings.Contains(s, "no such") {
		return nil
	}
	return fmt.Errorf("tmux kill-window %s: %w: %s", windowID, err, s)
}

// wrapExitErr unwraps *exec.ExitError so the stderr it carries shows
// up in error messages. exec.Command(...).Output() captures stderr
// in ExitError.Stderr but fmt.Errorf("%w", err) doesn't include it.
func wrapExitErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// shellJoin quotes argv so it can be passed as a single shell-command
// string to tmux new-window / send-keys. Uses POSIX single-quote
// semantics: any character other than ' is safe inside '...', and
// embedded ' is escaped as '\” (close-quote, escaped quote,
// reopen-quote). Arguments containing only safe characters are
// emitted bare.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '_', '-', '/', '.', '=', ':', ',', '@', '+':
			continue
		}
		safe = false
		break
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// MCPTimeoutEnvVar is Claude Code's bound, in milliseconds, on an MCP server
// attaching at session start. It does not retry: a server that misses the
// bound stays disconnected for the life of the session.
const MCPTimeoutEnvVar = "MCP_TIMEOUT"

// DefaultSeatMCPTimeoutMS is what a seat is launched with when the host has
// not chosen a value. Claude Code's own default is 30 s, measured from a
// spawn that on a loaded host is itself starved: on 2026-09-22 a jevons
// overseer and its PO each came up three times with bullseye in
// CONNECT_TIMEOUT while the same bridge answered initialize and tools/list in
// 10 ms from a shell, and could not file or achieve a target until relaunched
// with this bound, on which bullseye attached first try.
const DefaultSeatMCPTimeoutMS = "180000"

// mcpTimeoutEnv is the MCP_TIMEOUT assignment a new window gets. A value the
// host already exports wins; getenv is a parameter so the choice is testable.
func mcpTimeoutEnv(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv(MCPTimeoutEnvVar)); v != "" {
		return MCPTimeoutEnvVar + "=" + v
	}
	return MCPTimeoutEnvVar + "=" + DefaultSeatMCPTimeoutMS
}

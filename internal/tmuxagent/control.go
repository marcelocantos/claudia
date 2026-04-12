// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Control is a long-lived tmux control-mode client attached to the
// claudia-anchor session. It parses %output notifications from tmux
// and delivers the decoded bytes for a specific pane to the Bytes
// channel.
//
// One Control instance serves one pane (one agent window). Events
// for other panes in the same session are silently discarded — this
// is inefficient when many agents share the same session, but
// simpler than building a fanout. T1.2 may revisit if the
// inefficiency matters in practice.
//
// Lifecycle:
//
//	c, err := DialControl(windowID)
//	for data := range c.Bytes() { ... }
//	c.Close()
//
// The Bytes channel closes when the tmux subprocess exits (either
// because Close was called, the window was killed, or the server
// went away).
type Control struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	paneID string // the pane we care about (e.g. "%3")
	bytes  chan []byte

	closeOnce sync.Once
	closeErr  error
	done      chan struct{}
}

// DialControl starts a tmux control-mode client attached to the
// claudia-anchor session and filters %output notifications for the
// given window's active pane. The returned Control delivers the
// pane's bytes on Bytes() until Close is called or the tmux
// subprocess exits.
//
// Dialing after the window has already produced output is safe —
// tmux replays the current pane contents as %output on attach, so
// the history is not lost even if DialControl races with Claude's
// startup burst.
func DialControl(windowID string) (*Control, error) {
	paneID, err := paneIDFor(windowID)
	if err != nil {
		return nil, err
	}

	sock := SocketPath()
	cmd := exec.Command("tmux", "-S", sock, "-C", "attach-session", "-t", anchorSessionName)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start tmux control: %w", err)
	}

	c := &Control{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		paneID: paneID,
		bytes:  make(chan []byte, 256),
		done:   make(chan struct{}),
	}

	go c.readLoop()
	return c, nil
}

// Bytes returns the channel of decoded pane bytes for this control
// client's target pane. The channel closes when the control-mode
// subprocess exits.
func (c *Control) Bytes() <-chan []byte { return c.bytes }

// PaneID returns the tmux pane ID this control client is filtering on.
func (c *Control) PaneID() string { return c.paneID }

// Close terminates the control-mode subprocess and releases
// resources. Idempotent and safe to call from multiple goroutines.
func (c *Control) Close() error {
	c.closeOnce.Do(func() {
		// Closing stdin signals tmux to disconnect cleanly.
		c.stdin.Close()
		// Wait for the read loop to drain before reaping.
		<-c.done
		c.closeErr = c.cmd.Wait()
	})
	return c.closeErr
}

func (c *Control) readLoop() {
	defer close(c.done)
	defer close(c.bytes)

	reader := bufio.NewReader(c.stdout)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			c.handleLine(strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			return
		}
	}
}

func (c *Control) handleLine(line string) {
	if !strings.HasPrefix(line, "%") {
		return
	}
	if rest, ok := strings.CutPrefix(line, "%output "); ok {
		pane, payload, ok := strings.Cut(rest, " ")
		if !ok || pane != c.paneID {
			return
		}
		decoded := decodeOutputEscape(payload)
		if len(decoded) == 0 {
			return
		}
		select {
		case c.bytes <- decoded:
		default:
			// Drop on full buffer. Consumer is lagging; terminal
			// byte stream is best-effort for display, and the
			// Agent's ring buffer will still deliver history via
			// SubscribeTerminal on demand.
		}
		return
	}
	// Other notifications (%begin/%end/%exit/%sessions-changed/...)
	// are silently ignored for M2. M3 will add %exit handling to
	// notice when the window dies.
}

// decodeOutputEscape decodes tmux's control-mode escape format used
// in %output notifications. Any byte outside the printable ASCII
// range (plus backslash itself) is emitted by tmux as a
// 3-digit octal escape \<ddd>. Backslash itself is emitted as \\
// in some tmux versions and \134 in others; the decoder handles
// both forms.
func decodeOutputEscape(s string) []byte {
	out := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '\\' {
			out = append(out, '\\')
			i += 2
			continue
		}
		if i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			n := (int(s[i+1]-'0') << 6) | (int(s[i+2]-'0') << 3) | int(s[i+3]-'0')
			if n <= 0xff {
				out = append(out, byte(n))
				i += 4
				continue
			}
		}
		// Malformed escape — emit literal backslash and continue
		// one byte at a time. Don't panic on ill-formed input.
		out = append(out, c)
		i++
	}
	return out
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// paneIDFor returns the tmux pane ID (e.g. "%3") of the active pane
// of the given window.
func paneIDFor(windowID string) (string, error) {
	sock := SocketPath()
	out, err := exec.Command(
		"tmux", "-S", sock,
		"display-message", "-p", "-t", windowID, "#{pane_id}",
	).Output()
	if err != nil {
		return "", fmt.Errorf("tmux display-message: %w", wrapExitErr(err))
	}
	return strings.TrimSpace(string(out)), nil
}

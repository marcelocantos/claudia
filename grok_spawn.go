// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/marcelocantos/claudia/internal/broker"
)

// grokStderrCaptureMax bounds the startup stderr kept for a failed grant.
// The handshake is one line; the tail is for the operator, not a transcript.
const grokStderrCaptureMax = 8 << 10

// grokUserToolDirs are the directories an interactive shell puts ahead of
// a brew service PATH. Prepended onto the grok child only, and only when
// the directory exists, so the daemon's own PATH stays system-first.
func grokUserToolDirs(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".grok", "bin"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
		filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, ".py", "bin"),
		filepath.Join(home, ".den", "bin"),
		filepath.Join(home, ".bun", "bin"),
	}
}

// grokChildEnv is the environment of a grok ACP child: the daemon (or
// in-process caller) environment, plus extra overrides, with user tool
// directories moved to the front of PATH.
func grokChildEnv(extra []string) []string {
	return repairGrokChildPATH(appendEnv(os.Environ(), extra))
}

func repairGrokChildPATH(env []string) []string {
	home := homeFromEnv(env)
	var front []string
	for _, dir := range grokUserToolDirs(home) {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		front = append(front, dir)
	}
	if len(front) == 0 {
		return env
	}
	drop := make(map[string]struct{}, len(front))
	for _, dir := range front {
		drop[dir] = struct{}{}
	}
	pathIdx := -1
	var rest []string
	for i, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || key != "PATH" {
			continue
		}
		pathIdx = i
		for _, p := range filepath.SplitList(val) {
			if p == "" {
				continue
			}
			if _, ok := drop[p]; ok {
				continue
			}
			rest = append(rest, p)
		}
		break
	}
	merged := strings.Join(append(front, rest...), string(os.PathListSeparator))
	if pathIdx < 0 {
		return append(env, "PATH="+merged)
	}
	// appendEnv returns os.Environ() itself when extra is empty. Environ
	// is already a fresh slice; replacing one entry does not change the
	// process environment.
	env[pathIdx] = "PATH=" + merged
	return env
}

func homeFromEnv(env []string) string {
	for _, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if ok && key == "HOME" && val != "" {
			return val
		}
	}
	home, _ := os.UserHomeDir()
	return home
}

// grokStderrCapture records a bounded copy of child stderr and logs each
// line at debug, which is what drainStderr used to do on its own.
type grokStderrCapture struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func newGrokStderrCapture() *grokStderrCapture {
	return &grokStderrCapture{done: make(chan struct{})}
}

func (c *grokStderrCapture) consume(r io.Reader) {
	defer close(c.done)
	// Read raw so a child that exits without a trailing newline is still
	// captured. Line splitting is only for the debug log.
	var tmp [4096]byte
	var pending []byte
	for {
		n, err := r.Read(tmp[:])
		if n > 0 {
			chunk := tmp[:n]
			c.append(chunk)
			pending = append(pending, chunk...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				slog.Debug("grok acp stderr", "line", string(pending[:i]))
				pending = pending[i+1:]
			}
			if len(pending) > 256*1024 {
				slog.Debug("grok acp stderr", "line", string(pending[:256]))
				pending = nil
			}
		}
		if err != nil {
			if len(pending) > 0 {
				slog.Debug("grok acp stderr", "line", string(pending))
			}
			return
		}
	}
}

func (c *grokStderrCapture) append(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf.Len() >= grokStderrCaptureMax {
		return
	}
	remain := grokStderrCaptureMax - c.buf.Len()
	if len(p) > remain {
		p = p[:remain]
	}
	_, _ = c.buf.Write(p)
}

func (c *grokStderrCapture) wait() string {
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// explainGrokSidecarHandshake wraps err when err or stderr carries a
// one-word ready handshake (`helper: sidecar said "error", want ready`).
// Other errors are returned unchanged so existing ACP failure text stays
// stable.
func explainGrokSidecarHandshake(err error, stderr string) error {
	if err == nil {
		return nil
	}
	blob := err.Error()
	if stderr != "" {
		blob += "\n" + stderr
	}
	const marker = `sidecar said "`
	idx := strings.Index(blob, marker)
	if idx < 0 {
		return err
	}
	rest := blob[idx+len(marker):]
	end := strings.Index(rest, `", want ready`)
	if end < 0 {
		return err
	}
	status := rest[:end]
	name := grokSidecarHelperName(blob, idx)
	out := fmt.Errorf("%w\n\n%s", err, grokSidecarDiagnosis(name, status))
	tail := strings.TrimSpace(stderr)
	if tail != "" && !strings.Contains(err.Error(), tail) {
		out = fmt.Errorf("%w\n\ngrok stderr:\n%s", out, tail)
	}
	return out
}

// grokSidecarHelperName is the token immediately before `sidecar said` on
// the same line, after Go error wrapping (`acp initialize: omp: …`).
func grokSidecarHelperName(blob string, markerAt int) string {
	lineStart := strings.LastIndex(blob[:markerAt], "\n") + 1
	prefix := strings.TrimSpace(blob[lineStart:markerAt])
	prefix = strings.TrimSuffix(prefix, ":")
	prefix = strings.TrimSpace(prefix)
	if i := strings.LastIndex(prefix, ":"); i >= 0 {
		prefix = strings.TrimSpace(prefix[i+1:])
	}
	if prefix == "" || strings.ContainsAny(prefix, " \t") {
		return "sidecar"
	}
	switch prefix {
	case "initialize", "acp", "error", "grok":
		return "sidecar"
	}
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '+' || r == '-' {
			continue
		}
		return "sidecar"
	}
	return prefix
}

func grokSidecarDiagnosis(name, status string) string {
	msg := fmt.Sprintf(
		"grok helper %q answered its ready handshake with %q (sidecar said %q, want ready). "+
			"The broker grant returns as soon as the helper answers, so a failure in milliseconds is this handshake, with the socket up and no model turn burned. "+
			"The helper log is %s. When that log contains `command not found: setsid`, omp is a Node process whose zsh startup eval cannot find the setsid binary; macOS does not ship one, and `brew install util-linux` does. "+
			"Lines reading `write EPIPE` are that process writing to the pipe the parent closed after the handshake returned %q. "+
			"Install setsid, then restart the daemon (`brew services restart claudia`). "+
			"This build also moves existing user tool directories to the front of the grok child's PATH (~/.grok/bin, ~/.local/bin, ~/go/bin, ~/.cargo/bin, ~/.py/bin, ~/.den/bin, ~/.bun/bin) so a brew service PATH, which keeps system directories first, resolves the same helpers an interactive shell does.",
		name, status, status, ompSidecarLogPath(), status,
	)
	if excerpt := ompSidecarLogExcerpt(); excerpt != "" {
		msg += "\n\n" + excerpt
	}
	return msg
}

func ompSidecarLogPath() string {
	dir, err := broker.StateDir()
	if err != nil || dir == "" {
		return "~/.local/state/claudia/omp-sidecar.log"
	}
	return filepath.Join(dir, "omp-sidecar.log")
}

// ompSidecarLogExcerpt quotes the setsid / EPIPE lines from the helper log
// when this machine has them. Colossus 2026-09-26: one `command not found:
// setsid`, then repeated Node `write EPIPE`.
func ompSidecarLogExcerpt() string {
	body := readFileTail(ompSidecarLogPath(), grokStderrCaptureMax)
	if body == "" {
		return ""
	}
	if !strings.Contains(body, "setsid") && !strings.Contains(body, "EPIPE") {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "setsid") || strings.Contains(line, "EPIPE") {
			lines = append(lines, line)
		}
		if len(lines) == 6 {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "omp-sidecar.log:\n" + strings.Join(lines, "\n")
}

func readFileTail(path string, max int) string {
	if path == "" || max <= 0 {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}

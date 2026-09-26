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

// setsidShim is a util-linux-compatible setsid for hosts that have no
// setsid binary. macOS is one. Grok and Cursor both exec omp, whose zsh
// startup eval calls `setsid` by name; Claude and Codex do not.
const setsidShim = `#!/bin/sh
# Claudia shim. The host has no setsid binary. perl calls setsid(2).
waitflag=0
forkflag=0
while [ $# -gt 0 ]; do
  case "$1" in
    -w|--wait) waitflag=1; shift ;;
    -f|--fork) forkflag=1; shift ;;
    -c|--ctty) shift ;;
    --) shift; break ;;
    -*) echo "setsid: unsupported option $1" >&2; exit 1 ;;
    *) break ;;
  esac
done
if [ $# -eq 0 ]; then
  echo "setsid: must provide a command" >&2
  exit 1
fi
perlbin=""
for c in /usr/bin/perl /usr/local/bin/perl; do
  if [ -x "$c" ]; then perlbin=$c; break; fi
done
if [ -z "$perlbin" ]; then
  perlbin=$(command -v perl 2>/dev/null || true)
fi
if [ -z "$perlbin" ]; then
  echo "setsid: perl is required for the claudia shim; brew install util-linux provides a native setsid" >&2
  exit 127
fi
if [ "$forkflag" = 1 ]; then
  exec "$perlbin" -MPOSIX -e 'my $pid = fork(); die "setsid: fork: $!\n" unless defined $pid; exit 0 if $pid; POSIX::setsid(); exec @ARGV or die "setsid: exec: $!\n"' -- "$@"
fi
if [ "$waitflag" = 1 ]; then
  exec "$perlbin" -MPOSIX -e 'POSIX::setsid(); my $pid = fork(); die "setsid: fork: $!\n" unless defined $pid; if ($pid) { waitpid($pid, 0); if (($? & 127) == 0) { exit($? >> 8) } else { exit(128 + ($? & 127)) } } exec @ARGV or die "setsid: exec: $!\n"' -- "$@"
fi
exec "$perlbin" -MPOSIX -e 'POSIX::setsid(); exec @ARGV or die "setsid: exec: $!\n"' -- "$@"
`

var setsidShimOnce sync.Mutex

// grokStderrCaptureMax bounds the startup stderr kept for a failed grant.
// The handshake is one line; the tail is for the operator, not a transcript.
const grokStderrCaptureMax = 8 << 10

// grokUserToolDirs are the directories an interactive shell puts ahead of
// a brew service PATH. Prepended onto Grok and Cursor children only, and
// only when the directory exists, so the daemon's own PATH stays
// system-first. Claude and Codex stay on the service PATH.
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

// providerChildEnv is the environment of an OMP provider child (Grok or
// Cursor): the daemon environment, plus extra overrides, with user tool
// directories at the front of PATH and a setsid shim when the host has
// no setsid binary.
func providerChildEnv(extra []string) []string {
	return ensureSetsid(repairGrokChildPATH(appendEnv(os.Environ(), extra)))
}

// grokChildEnv is providerChildEnv. Grok and Cursor share the omp helper.
func grokChildEnv(extra []string) []string {
	return providerChildEnv(extra)
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

// ensureSetsid adds a setsid shim when the child PATH cannot already
// execute setsid. User tool directories stay ahead of the shim so
// ~/.grok/bin still wins. A host setsid is left where it is.
func ensureSetsid(env []string) []string {
	if lookupExecutable(envValue(env, "PATH"), "setsid") != "" {
		return env
	}
	dir, err := installSetsidShim()
	if err != nil || dir == "" {
		return env
	}
	return insertPathAfterUserBins(env, dir)
}

// insertPathAfterUserBins places dir after the leading user tool
// directories and before the rest of PATH.
func insertPathAfterUserBins(env []string, dir string) []string {
	user := map[string]struct{}{}
	for _, d := range grokUserToolDirs(homeFromEnv(env)) {
		user[d] = struct{}{}
	}
	var parts []string
	pathIdx := -1
	for i, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || key != "PATH" {
			continue
		}
		pathIdx = i
		for _, p := range filepath.SplitList(val) {
			if p == "" || p == dir {
				continue
			}
			parts = append(parts, p)
		}
		break
	}
	at := 0
	for at < len(parts) {
		if _, ok := user[parts[at]]; !ok {
			break
		}
		at++
	}
	merged := make([]string, 0, len(parts)+1)
	merged = append(merged, parts[:at]...)
	merged = append(merged, dir)
	merged = append(merged, parts[at:]...)
	path := strings.Join(merged, string(os.PathListSeparator))
	if pathIdx < 0 {
		return append(env, "PATH="+path)
	}
	env[pathIdx] = "PATH=" + path
	return env
}

func installSetsidShim() (string, error) {
	state, err := broker.StateDir()
	if err != nil || state == "" {
		return "", err
	}
	dir := filepath.Join(state, "bin")
	path := filepath.Join(dir, "setsid")
	setsidShimOnce.Lock()
	defer setsidShimOnce.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	existing, readErr := os.ReadFile(path)
	if readErr != nil || string(existing) != setsidShim {
		if err := os.WriteFile(path, []byte(setsidShim), 0o755); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func lookupExecutable(pathEnv, name string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		fi, err := os.Stat(candidate)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
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
		out = fmt.Errorf("%w\n\nchild stderr:\n%s", out, tail)
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
		"helper %q answered its ready handshake with %q (sidecar said %q, want ready). "+
			"Grok and Cursor broker seats both start this omp helper. Claude and Codex do not. "+
			"The grant returns as soon as the helper answers, so a failure in milliseconds is this handshake, with the socket up and no model turn burned. "+
			"The helper log is %s. When that log contains `command not found: setsid`, omp is a Node process whose zsh startup eval cannot find the setsid binary. macOS does not ship one. "+
			"This build places a setsid shim on the Grok and Cursor child PATH when the host has none; the shim uses perl to call setsid(2). `brew install util-linux` installs a native setsid when perl cannot run the shim. "+
			"Lines reading `write EPIPE` are that process writing to the pipe the parent closed after the handshake returned %q. "+
			"Restart the daemon (`brew services restart claudia`) so the next Grok or Cursor grant is spawned by this build. "+
			"Existing user tool directories are also moved to the front of that child PATH (~/.grok/bin, ~/.local/bin, ~/go/bin, ~/.cargo/bin, ~/.py/bin, ~/.den/bin, ~/.bun/bin).",
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

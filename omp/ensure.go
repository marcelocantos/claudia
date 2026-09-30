// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	// SocketEnv names the sidecar unix socket. Empty means the default
	// under the claudia state directory.
	SocketEnv   = "CLAUDIA_OMP_SOCKET"
	socketName  = "omp.sock"
	stateSubdir = "claudia"
)

// SocketPath is the sidecar's listen address. CLAUDIA_OMP_SOCKET wins.
func SocketPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(SocketEnv)); p != "" {
		return filepath.Abs(p)
	}
	if testing.Testing() {
		// A test never reaches the owner's sidecar, nor starts one on its
		// socket (🎯T145, the 🎯T143 class): set CLAUDIA_OMP_SOCKET.
		return "", errors.New("omp: a test binary never uses the real sidecar socket; set " + SocketEnv + " (🎯T145)")
	}
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, socketName), nil
}

func stateDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); home != "" && filepath.IsAbs(home) {
		return filepath.Join(home, stateSubdir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("omp: locate state dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", stateSubdir), nil
}

// ServerScript is sidecar/server.ts next to this module, or CLAUDIA_OMP_SERVER.
func ServerScript() string {
	if p := os.Getenv("CLAUDIA_OMP_SERVER"); p != "" {
		return p
	}
	if dir := PackagedSidecarDir(); dir != "" {
		return filepath.Join(dir, "server.ts")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "sidecar/server.ts"
	}
	return filepath.Join(filepath.Dir(file), "..", "sidecar", "server.ts")
}

// Listening reports whether the sidecar already accepts connections.
// The dial is bounded on its own so a leftover socket file cannot
// consume the caller's whole deadline (unix dial hangs until accept
// when the file exists but nobody is listening).
func Listening(ctx context.Context, path string) bool {
	dialCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(dialCtx, "unix", path)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

var ensureMu sync.Mutex

// LifelineArg tells the sidecar that its stdin is a lifeline from the
// process that started it: it exits when that pipe reaches EOF (🎯T166).
const LifelineArg = "--lifeline=stdin"

// started holds the pids of the sidecars this process started (under
// ensureMu). StopUnowned leaves them alone.
var started = map[int]bool{}

// Ensure starts a Bun sidecar if the socket is not already accepting.
//
// The sidecar is this process's child and lives only as long as it
// (🎯T166). Its stdin is a pipe whose write end only this process holds;
// when this process exits, however it exits, the kernel closes that end
// and the sidecar reads EOF and exits too. A sidecar is part of the
// broker that started it, never an orphan that a later broker adopts:
// adopting one kept an older build serving every seat.
func Ensure(ctx context.Context) (string, error) {
	path, err := SocketPath()
	if err != nil {
		return "", err
	}
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if Listening(ctx, path) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("omp: sidecar socket dir: %w", err)
	}
	// No unlink here (🎯T145): a sidecar that is alive but not yet
	// listening owns this path. The sidecar removes a stale socket itself,
	// and only while holding the socket's lock.
	script := ServerScript()
	if _, err := os.Stat(script); err != nil {
		return "", fmt.Errorf("omp: sidecar script %s: %w", script, err)
	}
	if err := EnsureDeps(ctx, script); err != nil {
		return "", err
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		return "", fmt.Errorf("omp: bun is required for the sidecar: %w", err)
	}
	logPath := filepath.Join(filepath.Dir(path), "omp-sidecar.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("omp: sidecar log: %w", err)
	}
	// os.Pipe is close-on-exec, so no other child of this process
	// inherits the write end and keeps the sidecar alive after us.
	lifeline, hold, err := os.Pipe()
	if err != nil {
		_ = logf.Close()
		return "", fmt.Errorf("omp: sidecar lifeline: %w", err)
	}
	cmd := exec.Command(bun, script, path, LifelineArg)
	cmd.Env = ScrubEnv(os.Environ())
	cmd.Dir = filepath.Dir(script)
	cmd.Stdin = lifeline
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		_ = lifeline.Close()
		_ = hold.Close()
		return "", fmt.Errorf("omp: start sidecar: %w", err)
	}
	_ = lifeline.Close()
	pid := cmd.Process.Pid
	started[pid] = true
	// The sidecar that wins the socket's lock writes the pid file; a
	// redundant start exits and must not touch it (🎯T145). hold stays
	// open until the child is gone: this goroutine keeps it reachable, so
	// no finalizer closes it early.
	go func() {
		_ = cmd.Wait()
		_ = hold.Close()
		_ = logf.Close()
		ensureMu.Lock()
		delete(started, pid)
		ensureMu.Unlock()
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if Listening(ctx, path) {
			return path, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", fmt.Errorf("omp: sidecar at %s did not become ready", path)
}

// unownedStopWait bounds how long StopUnowned waits for a sidecar to let go
// of the socket after SIGTERM, and again after SIGKILL.
const unownedStopWait = 10 * time.Second

// StopUnowned stops a sidecar that serves the socket but was not started
// by this process, and reports its pid (0 when there was none). A broker
// calls it once it holds the broker socket and before any seat resumes
// (🎯T166), so every seat lands on the broker's own sidecar: one left
// behind by an earlier broker may be an older build.
//
// The socket's lock (🎯T145) is the evidence: a sidecar holds it for its
// whole life and the kernel drops it on exit, so a free lock means no
// sidecar is serving, whatever the pid file says.
func StopUnowned(ctx context.Context) (int, error) {
	path, err := SocketPath()
	if err != nil {
		return 0, err
	}
	ensureMu.Lock()
	defer ensureMu.Unlock()
	free, err := socketLockFree(path)
	if err != nil || free {
		return 0, err
	}
	raw, err := os.ReadFile(pidPath(path))
	if err != nil {
		return 0, fmt.Errorf("omp: a sidecar holds %s but its pid file is unreadable: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("omp: a sidecar holds %s but its pid file says %q", path, raw)
	}
	if started[pid] {
		return 0, nil
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return pid, fmt.Errorf("omp: stop sidecar %d: %w", pid, err)
		}
		deadline := time.Now().Add(unownedStopWait)
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return pid, ctx.Err()
			}
			if free, err := socketLockFree(path); err != nil || free {
				return pid, err
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return pid, fmt.Errorf("omp: sidecar %d still holds %s after SIGKILL", pid, path)
}

// socketLockFree reports whether no sidecar holds the socket's lock. It
// takes the lock and releases it at once, so it never keeps a sidecar out.
func socketLockFree(path string) (bool, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("omp: sidecar lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("omp: sidecar lock: %w", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true, nil
}

func pidPath(socket string) string { return socket + ".pid" }

func itoa(n int) string { return strconv.Itoa(n) }

// StopSidecar kills the process named by the socket's pid file. Tests and
// isolated brokers use it to stop a sidecar before the process that
// started it exits.
func StopSidecar(socket string) error {
	raw, err := os.ReadFile(pidPath(socket))
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("omp: bad sidecar pid %q", raw)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	_ = os.Remove(socket)
	_ = os.Remove(pidPath(socket))
	return nil
}

// PackagedSidecarDir is the sidecar a release installs beside the binary
// (<prefix>/share/claudia/sidecar for <prefix>/bin/claudia), or "" when this
// binary has none, as in a source checkout (🎯T157).
func PackagedSidecarDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return packagedSidecarDir(exe)
}

func packagedSidecarDir(exe string) string {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Join(filepath.Dir(exe), "..", "share", "claudia", "sidecar")
	if _, err := os.Stat(filepath.Join(dir, "server.ts")); err != nil {
		return ""
	}
	return filepath.Clean(dir)
}

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

// Ensure starts a detached Bun sidecar if the socket is not already
// accepting. A jevonsd (or even broker) bounce leaves that process
// running: the child is in its own session and is not waited on.
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
	bun, err := exec.LookPath("bun")
	if err != nil {
		return "", fmt.Errorf("omp: bun is required for the sidecar: %w", err)
	}
	cmd := exec.Command(bun, script, path)
	cmd.Env = ScrubEnv(os.Environ())
	cmd.Dir = filepath.Dir(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logPath := filepath.Join(filepath.Dir(path), "omp-sidecar.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("omp: sidecar log: %w", err)
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return "", fmt.Errorf("omp: start sidecar: %w", err)
	}
	// The sidecar that wins the socket's lock writes the pid file; a
	// redundant start exits and must not touch it (🎯T145).
	go func() {
		_ = cmd.Wait()
		_ = logf.Close()
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

func pidPath(socket string) string { return socket + ".pid" }

func itoa(n int) string { return strconv.Itoa(n) }

// StopSidecar kills the process named by the socket's pid file. Tests
// use it so a detached Ensure does not leak. Production restarts the
// sidecar deliberately; a jevonsd bounce must not call this.
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

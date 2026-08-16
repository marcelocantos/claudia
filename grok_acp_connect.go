// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// EnvGrokConnect, when truthy ("1", "true", "yes", "on"), enables Grok
// connect-mode for Session Start even when Config.GrokConnect is false.
// Connect-mode runs a detached `grok agent serve` and speaks ACP over
// WebSocket so the agent process outlives the consumer (jevons 🎯T40).
const EnvGrokConnect = "CLAUDIA_GROK_CONNECT"

// grokConnectEnabled reports whether connect-mode should be used for Grok.
func grokConnectEnabled(cfg Config) bool {
	if cfg.GrokConnect {
		return true
	}
	// Explicit reattach always uses connect-mode.
	if cfg.ConnectURL != "" {
		return true
	}
	return truthyEnv(os.Getenv(EnvGrokConnect))
}

func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// startGrokACPConnect starts or reattaches a durable Grok serve process and
// returns a ready ACP client over WebSocket.
//
// When cfg.ConnectURL is set and cfg.ConnectPID is alive, reattaches only
// (no new process). If reattach dial/session fails (zombie PID, half-dead
// serve after Stop), kills the stale PID and falls through to spawn — a
// failed reattach must not strand the caller with no process.
// Otherwise spawns `grok agent serve` detached, dials it, and opens a session.
func startGrokACPConnect(bin string, workDir, model, sessionID string, requireResume bool, mcpServers []any, cfg Config, onEvent func(Event), onClose func()) (*grokACPClient, error) {
	url := strings.TrimSpace(cfg.ConnectURL)
	pid := cfg.ConnectPID

	if url != "" && processAlive(pid) {
		c, err := dialGrokServe(url, pid, true, onEvent, onClose)
		if err == nil {
			c.sessionID = sessionID
			if err = c.initialize(); err == nil {
				if err = c.openSession(workDir, sessionID, requireResume, mcpServers); err == nil {
					return c, nil
				}
			}
			c.Close()
		}
		// Reattach failed while processAlive was true (dying serve, reset
		// peer, wrong key). Kill and spawn rather than failing closed.
		slog.Warn("grok connect reattach failed; killing stale serve and spawning new",
			"url", url, "pid", pid, "err", err)
		_ = killPID(pid)
	} else if url != "" {
		// Stale connect endpoint: fall through to spawn.
		slog.Info("grok connect: prior serve not alive; spawning new",
			"url", url, "pid", pid)
	}

	serve, err := spawnDetachedGrokServe(bin, model)
	if err != nil {
		return nil, err
	}
	c, err := dialGrokServe(serve.URL, serve.PID, true, onEvent, onClose)
	if err != nil {
		_ = killPID(serve.PID)
		return nil, fmt.Errorf("grok connect dial after spawn: %w", err)
	}
	// Hold cmd nil; we own via connectPID.
	c.cmd = nil
	if err := c.initialize(); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openSession(workDir, sessionID, requireResume, mcpServers); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// grokServeEndpoint is a running detached serve process.
type grokServeEndpoint struct {
	URL string
	PID int
}

// spawnDetachedGrokServe starts `grok agent serve` in a new session so it
// survives consumer exit, waits until the port accepts connections, and
// returns the WebSocket URL + PID.
func spawnDetachedGrokServe(bin, model string) (*grokServeEndpoint, error) {
	port, err := freeTCPPort()
	if err != nil {
		return nil, fmt.Errorf("grok serve free port: %w", err)
	}
	secret, err := randomSecret(16)
	if err != nil {
		return nil, fmt.Errorf("grok serve secret: %w", err)
	}
	bind := fmt.Sprintf("127.0.0.1:%d", port)
	args := append(grokACPArgs(model, true), "--bind", bind, "--secret", secret)

	cmd := exec.Command(bin, args...)
	// Detach: new session so SIGHUP on consumer death does not kill serve.
	// Stdio discarded — ACP is over WebSocket, not these pipes.
	detachProcess(cmd)
	logPath := serveLogPath(port)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
			// File closed by OS when process exits; do not Close here.
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start grok serve: %w", err)
	}
	pid := cmd.Process.Pid
	// Release so we do not wait on exit; process is reparented.
	_ = cmd.Process.Release()

	url := fmt.Sprintf("ws://127.0.0.1:%d/ws?server-key=%s", port, secret)
	if err := waitTCP(bind, 15*time.Second); err != nil {
		_ = killPID(pid)
		return nil, fmt.Errorf("grok serve not ready on %s (pid %d): %w", bind, pid, err)
	}
	slog.Info("grok serve started (connect-mode)", "pid", pid, "bind", bind, "log", logPath)
	return &grokServeEndpoint{URL: url, PID: pid}, nil
}

func serveLogPath(port int) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "claudia", "grok-serve", strconv.Itoa(port)+".log")
}

func dialGrokServe(url string, pid int, ownsProcess bool, onEvent func(Event), onClose func()) (*grokACPClient, error) {
	ctx, cancel := context.WithCancel(context.Background())
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	conn, _, err := websocket.Dial(dialCtx, url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("websocket dial %s: %w", url, err)
	}
	conn.SetReadLimit(4 << 20)

	c := &grokACPClient{
		ws:          conn,
		wsCtx:       ctx,
		wsCancel:    cancel,
		ownsProcess: ownsProcess,
		connectURL:  url,
		connectPID:  pid,
		pending:     make(map[int64]chan acpRPCMessage),
		onEvent:     onEvent,
		onClose:     onClose,
	}
	go c.readLoop()
	return c, nil
}

func freeTCPPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return last
}

func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// processAlive reports whether pid is a live OS process (best-effort).
// Overridable in tests.
var processAlive = processAliveImpl

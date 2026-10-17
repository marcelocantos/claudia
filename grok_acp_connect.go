// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
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

// startGrokACPConnect refuses a CLI ACP client. It used to start or
// reattach a detached `grok agent serve` process and return a WebSocket
// client.
func startGrokACPConnect(bin string, workDir, model, sessionID string, requireResume bool, mcpServers []any, cfg Config, extraEnv []string, onEvent func(Event), onClose func()) (*grokACPClient, error) {
	return nil, fmt.Errorf("%w: %s", ErrLegacyCLISeatTransport, ProviderGrok)
}

// grokServeEndpoint is a running detached serve process.
type grokServeEndpoint struct {
	URL string
	PID int
	// Log is the file the detached serve writes stdout and stderr to.
	Log string
}

// spawnDetachedGrokServe refuses. It used to start `grok agent serve`
// in a new session and return the WebSocket URL and PID.
func spawnDetachedGrokServe(bin, model string, extraEnv []string) (*grokServeEndpoint, error) {
	return nil, fmt.Errorf("%w: %s", ErrLegacyCLISeatTransport, ProviderGrok)
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

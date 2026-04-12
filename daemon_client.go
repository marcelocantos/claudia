// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/marcelocantos/claudia/internal/daemonproto"
)

// ErrDaemonUnavailable is returned by [LookupChain] when the claudiad
// daemon is not reachable. This is deliberately a loud error rather
// than a degenerate "success" that returns a single-element chain —
// the daemon is the authoritative source of session-chain linkage,
// and silently lying to callers who trust that authority would be
// worse than forcing them to handle the missing-daemon case
// explicitly. Consumers that want the degenerate behaviour can write:
//
//	chain, err := claudia.LookupChain(sid)
//	if errors.Is(err, claudia.ErrDaemonUnavailable) {
//	    chain = []string{sid}
//	}
var ErrDaemonUnavailable = errors.New("claudia: daemon unavailable")

// DaemonSocketPath returns the path claudia expects the daemon's
// Unix socket at. Useful for diagnostics. Defaults to
// $XDG_STATE_HOME/claudia/claudiad.sock.
func DaemonSocketPath() string { return daemonproto.SocketPath() }

// daemonDialBudget is the timeout for opportunistic daemon dials
// during Agent startup. Kept short so startup stays fast when no
// daemon is running.
const daemonDialBudget = 250 * time.Millisecond

// daemonClient is a minimal WebSocket + JSON-RPC 2.0 client used by
// the library to talk to a running claudiad. It is intentionally
// barebones — no subscription support, no reconnect — because Slice 1
// only uses fire-and-forget Register and request/response Lookup.
type daemonClient struct {
	conn *websocket.Conn

	mu      sync.Mutex
	nextID  atomic.Int64
	closed  bool
}

// dialDaemon attempts to open a WebSocket connection to the daemon's
// Unix domain socket. The ctx budget should be kept short — startup
// is allowed to degrade gracefully to direct-spawn mode.
func dialDaemon(ctx context.Context) (*daemonClient, error) {
	sock := daemonproto.SocketPath()
	// coder/websocket's Dial uses an HTTP client under the hood.
	// Hand it a transport that dials the Unix socket and speaks HTTP
	// against http://claudiad/rpc. The host is a placeholder — the
	// dialer ignores it.
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	conn, _, err := websocket.Dial(ctx, "http://claudiad/rpc", &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, err
	}
	return &daemonClient{conn: conn}, nil
}

// Close terminates the WebSocket. Safe to call more than once.
func (c *daemonClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
}

// call sends a JSON-RPC request and waits for the matching response.
// Slice 1 uses a simple single-in-flight model protected by mu; no
// concurrent calls on the same client.
func (c *daemonClient) call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("daemon client closed")
	}

	id := c.nextID.Add(1)
	idRaw, _ := json.Marshal(id)

	var paramsRaw json.RawMessage
	if params != nil {
		pb, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		paramsRaw = pb
	}
	req := daemonproto.Request{
		JSONRPC:         "2.0",
		ClaudiadVersion: daemonproto.Version,
		ID:              idRaw,
		Method:          method,
		Params:          paramsRaw,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// Read until we see our response. Skip notifications — Slice 1
	// doesn't subscribe on the library side, so any notifications
	// are just noise.
	for {
		_, msg, err := c.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var resp daemonproto.Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
		// Notification: no ID field.
		if len(resp.ID) == 0 {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("daemon error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("unmarshal result: %w", err)
			}
		}
		return nil
	}
}

// registerSession reports (cwd, session_id, pid) to the daemon.
// Best-effort: errors are logged and swallowed by the caller.
func (c *daemonClient) registerSession(ctx context.Context, cwd, sid string, pid int) error {
	return c.call(ctx, daemonproto.MethodSessionRegister,
		daemonproto.SessionRegisterParams{Cwd: cwd, SessionID: sid, PID: pid},
		&daemonproto.SessionRegisterResult{})
}

// lookupChain asks the daemon to resolve a session ID's full chain.
func (c *daemonClient) lookupChain(ctx context.Context, sid string) ([]string, string, error) {
	var result daemonproto.SessionLookupChainResult
	err := c.call(ctx, daemonproto.MethodSessionLookupChain,
		daemonproto.SessionLookupChainParams{SessionID: sid}, &result)
	if err != nil {
		return nil, "", err
	}
	return result.Chain, result.Confidence, nil
}

// LookupChain returns the full session chain containing sid, querying
// the claudiad daemon. The returned chain is ordered from oldest to
// newest (the original session ID first). For a session that has
// never rolled over, the chain contains a single element.
//
// If the daemon is not running or unreachable, returns [ErrDaemonUnavailable].
// The second return value carries the chain's confidence — "deterministic"
// when the daemon attributed every link unambiguously, or "ambiguous"
// when a /clear rollover happened in a cwd with multiple concurrent
// consumers.
func LookupChain(sid string) ([]string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, err := dialDaemon(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrDaemonUnavailable, err)
	}
	defer client.Close()

	chain, conf, err := client.lookupChain(ctx, sid)
	if err != nil {
		return nil, "", err
	}
	return chain, conf, nil
}

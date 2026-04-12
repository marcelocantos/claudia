// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/marcelocantos/claudia/internal/daemonproto"
)

// Server is the claudiad WebSocket + JSON-RPC 2.0 endpoint. It owns
// a [State] and a [Watcher] and multiplexes incoming WebSocket
// connections over a single Unix domain socket.
type Server struct {
	state   *State
	watcher *Watcher

	socketPath string
	listener   net.Listener
	httpServer *http.Server

	startOnce sync.Once
	stopOnce  sync.Once
	stopped   chan struct{}
}

// Config configures a Server.
type Config struct {
	// SocketPath overrides the Unix domain socket path. Defaults to
	// daemonproto.SocketPath().
	SocketPath string

	// ProjectsDir overrides the Claude Code projects directory root.
	// Defaults to ~/.claude/projects.
	ProjectsDir string

	// PendingTTL overrides the State pending-observation TTL.
	PendingTTL time.Duration

	// SweepInterval overrides the State periodic sweep interval.
	SweepInterval time.Duration
}

// NewServer constructs a Server from a Config. Call Start to begin
// serving; Stop to shut it down. The server owns the lifecycle of
// the underlying [State] and [Watcher].
func NewServer(cfg Config) (*Server, error) {
	var opts []Option
	if cfg.ProjectsDir != "" {
		opts = append(opts, WithProjectsDir(cfg.ProjectsDir))
	}
	if cfg.PendingTTL > 0 {
		opts = append(opts, WithPendingTTL(cfg.PendingTTL))
	}
	if cfg.SweepInterval > 0 {
		opts = append(opts, WithSweepInterval(cfg.SweepInterval))
	}

	state := NewState(opts...)
	watcher, err := NewWatcher(state)
	if err != nil {
		return nil, fmt.Errorf("daemon: new watcher: %w", err)
	}

	sockPath := cfg.SocketPath
	if sockPath == "" {
		sockPath = daemonproto.SocketPath()
	}

	return &Server{
		state:      state,
		watcher:    watcher,
		socketPath: sockPath,
		stopped:    make(chan struct{}),
	}, nil
}

// Start begins serving. The parent directory of the socket is created
// with 0700 permissions (so a co-tenant can't replace the socket
// file), and the socket itself is chmod'd to 0600 after bind.
func (s *Server) Start() error {
	var startErr error
	s.startOnce.Do(func() {
		// Ensure socket parent dir with restrictive permissions.
		parent := filepath.Dir(s.socketPath)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			startErr = fmt.Errorf("daemon: mkdir socket parent: %w", err)
			return
		}
		if err := os.Chmod(parent, 0o700); err != nil {
			startErr = fmt.Errorf("daemon: chmod socket parent: %w", err)
			return
		}

		// Remove a stale socket file from a previous crashed run.
		// Only remove if it isn't currently listenable (a simple
		// connect test). We don't use os.Stat because a live socket
		// shows up as a file in that call too.
		if _, err := os.Stat(s.socketPath); err == nil {
			conn, err := net.DialTimeout("unix", s.socketPath, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				startErr = fmt.Errorf("daemon: socket %s already in use", s.socketPath)
				return
			}
			if rerr := os.Remove(s.socketPath); rerr != nil {
				startErr = fmt.Errorf("daemon: remove stale socket: %w", rerr)
				return
			}
		}

		l, err := net.Listen("unix", s.socketPath)
		if err != nil {
			startErr = fmt.Errorf("daemon: listen: %w", err)
			return
		}
		if err := os.Chmod(s.socketPath, 0o600); err != nil {
			_ = l.Close()
			startErr = fmt.Errorf("daemon: chmod socket: %w", err)
			return
		}
		s.listener = l

		if err := s.watcher.Start(); err != nil {
			_ = l.Close()
			startErr = fmt.Errorf("daemon: start watcher: %w", err)
			return
		}
		s.state.Start()

		mux := http.NewServeMux()
		mux.HandleFunc("/rpc", s.handleRPC)
		s.httpServer = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}

		go func() {
			if err := s.httpServer.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("daemon http server", "err", err)
			}
			close(s.stopped)
		}()
	})
	return startErr
}

// Stop shuts the server down. Idempotent. Blocks until the HTTP
// server has exited.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		if s.httpServer != nil {
			err = s.httpServer.Shutdown(ctx)
		}
		if s.watcher != nil {
			s.watcher.Close()
		}
		if s.state != nil {
			s.state.Close()
		}
		// Remove the socket file so the next run can rebind.
		_ = os.Remove(s.socketPath)
		if s.stopped != nil {
			<-s.stopped
		}
	})
	return err
}

// SocketPath returns the resolved socket path this server is
// listening on.
func (s *Server) SocketPath() string { return s.socketPath }

// State exposes the underlying state for tests.
func (s *Server) State() *State { return s.state }

// handleRPC is the HTTP handler that accepts a WebSocket and runs
// the JSON-RPC loop for that connection.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Accept from anywhere — this is a local Unix socket, access
		// control is filesystem permissions.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("daemon accept", "err", err)
		return
	}
	defer conn.Close(websocket.StatusInternalError, "")

	ctx := r.Context()
	s.serveConn(ctx, conn)
	conn.Close(websocket.StatusNormalClosure, "")
}

// serveConn runs the read/write loops for a single connection. Each
// connection has its own writer goroutine so chain.update pushes
// from other goroutines don't race with request responses.
func (s *Server) serveConn(ctx context.Context, conn *websocket.Conn) {
	writes := make(chan daemonproto.Response, 32)
	// subs collects the per-connection subscriptions so we can clean
	// them up on disconnect, preventing the classic daemon leak.
	var subsMu sync.Mutex
	subs := make(map[string]struct{})

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for resp := range writes {
			data, err := json.Marshal(resp)
			if err != nil {
				slog.Warn("daemon marshal response", "err", err)
				continue
			}
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := conn.Write(writeCtx, websocket.MessageText, data); err != nil {
				cancel()
				return
			}
			cancel()
		}
	}()

	// Reader loop.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var req daemonproto.Request
		if err := json.Unmarshal(data, &req); err != nil {
			writes <- errorResponse(nil, daemonproto.ErrorCodeParse, "parse error")
			continue
		}
		resp := s.dispatch(&req, writes, &subsMu, subs)
		if resp != nil {
			writes <- *resp
		}
	}

	// Connection teardown — drop any surviving subscriptions.
	subsMu.Lock()
	for id := range subs {
		s.state.Unsubscribe(id)
	}
	subsMu.Unlock()

	close(writes)
	<-writerDone
}

// dispatch routes a single request. Returns the response envelope to
// send back, or nil if the response was (or will be) emitted out of
// band (e.g. subscription streams).
func (s *Server) dispatch(
	req *daemonproto.Request,
	writes chan<- daemonproto.Response,
	subsMu *sync.Mutex,
	subs map[string]struct{},
) *daemonproto.Response {
	switch req.Method {
	case daemonproto.MethodSessionRegister:
		var p daemonproto.SessionRegisterParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			r := errorResponse(req.ID, daemonproto.ErrorCodeInvalidParams, err.Error())
			return &r
		}
		s.state.Register(p.Cwd, p.SessionID, p.PID)
		return okResponse(req.ID, daemonproto.SessionRegisterResult{OK: true})

	case daemonproto.MethodSessionLookupChain:
		var p daemonproto.SessionLookupChainParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			r := errorResponse(req.ID, daemonproto.ErrorCodeInvalidParams, err.Error())
			return &r
		}
		chain, conf, ok := s.state.LookupChain(p.SessionID)
		if !ok {
			r := errorResponse(req.ID, daemonproto.ErrorCodeInternal, "unknown session")
			return &r
		}
		return okResponse(req.ID, daemonproto.SessionLookupChainResult{
			Chain:      chain,
			Confidence: conf,
		})

	case daemonproto.MethodSessionSubscribeChainUpdates:
		subID, ch := s.state.Subscribe()
		subsMu.Lock()
		subs[subID] = struct{}{}
		subsMu.Unlock()

		// Spawn a goroutine that forwards updates from this
		// subscription's channel into the connection's writer.
		// It exits when the state closes the channel (on Unsubscribe).
		go func() {
			for u := range ch {
				params, err := json.Marshal(u)
				if err != nil {
					continue
				}
				writes <- daemonproto.Response{
					JSONRPC:         "2.0",
					ClaudiadVersion: daemonproto.Version,
					Method:          daemonproto.NotificationChainUpdate,
					Params:          params,
				}
			}
		}()

		return okResponse(req.ID, daemonproto.SessionSubscribeResult{SubscriptionID: subID})

	case daemonproto.MethodSessionUnsubscribeChainUpdates:
		var p daemonproto.SessionUnsubscribeParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			r := errorResponse(req.ID, daemonproto.ErrorCodeInvalidParams, err.Error())
			return &r
		}
		subsMu.Lock()
		delete(subs, p.SubscriptionID)
		subsMu.Unlock()
		s.state.Unsubscribe(p.SubscriptionID)
		return okResponse(req.ID, daemonproto.SessionUnsubscribeResult{OK: true})

	default:
		r := errorResponse(req.ID, daemonproto.ErrorCodeMethodNotFound, "unknown method: "+req.Method)
		return &r
	}
}

func okResponse(id json.RawMessage, result any) *daemonproto.Response {
	data, err := json.Marshal(result)
	if err != nil {
		r := errorResponse(id, daemonproto.ErrorCodeInternal, err.Error())
		return &r
	}
	return &daemonproto.Response{
		JSONRPC:         "2.0",
		ClaudiadVersion: daemonproto.Version,
		ID:              id,
		Result:          data,
	}
}

func errorResponse(id json.RawMessage, code int, msg string) daemonproto.Response {
	return daemonproto.Response{
		JSONRPC:         "2.0",
		ClaudiadVersion: daemonproto.Version,
		ID:              id,
		Error: &daemonproto.Error{
			Code:    code,
			Message: msg,
		},
	}
}

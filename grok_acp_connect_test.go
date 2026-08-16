// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGrokConnectEnabled(t *testing.T) {
	t.Setenv(EnvGrokConnect, "")
	if grokConnectEnabled(Config{}) {
		t.Fatal("default off")
	}
	if !grokConnectEnabled(Config{GrokConnect: true}) {
		t.Fatal("GrokConnect true")
	}
	if !grokConnectEnabled(Config{ConnectURL: "ws://x"}) {
		t.Fatal("ConnectURL implies connect")
	}
	t.Setenv(EnvGrokConnect, "1")
	if !grokConnectEnabled(Config{}) {
		t.Fatal("env enables")
	}
}

func TestFreeTCPPortAndWait(t *testing.T) {
	port, err := freeTCPPort()
	if err != nil {
		t.Fatal(err)
	}
	if port <= 0 {
		t.Fatalf("port %d", port)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
		_ = ln.Close()
	}()
	if err := waitTCP(addr, 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

// fakeACPServe is a minimal WebSocket ACP server for hermetic connect tests.
func startFakeACPServe(t *testing.T) (url string, shutdown func()) {
	t.Helper()
	var mu sync.Mutex
	sessions := map[string]bool{}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(data, &req); err != nil {
				continue
			}
			id, _ := req["id"].(float64)
			method, _ := req["method"].(string)
			// notifications have no id
			if method == "notifications/initialized" || method == "initialized" {
				continue
			}
			if id == 0 && method != "" {
				continue
			}
			var result any
			switch method {
			case "initialize":
				result = map[string]any{
					"protocolVersion": 1,
					"agentCapabilities": map[string]any{
						"loadSession": true,
					},
					"serverInfo": map[string]any{"name": "fake", "version": "0"},
				}
			case "session/new":
				sid := "sess-fake-new"
				mu.Lock()
				sessions[sid] = true
				mu.Unlock()
				result = map[string]any{"sessionId": sid}
			case "session/load":
				params, _ := req["params"].(map[string]any)
				sid, _ := params["sessionId"].(string)
				mu.Lock()
				ok := sessions[sid] || sid != ""
				if ok {
					sessions[sid] = true
				}
				mu.Unlock()
				if !ok {
					_ = conn.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"error":   map[string]any{"code": -32000, "message": "unknown session"},
					}))
					continue
				}
				result = map[string]any{"sessionId": sid}
			case "session/prompt":
				result = map[string]any{"stopReason": "end_turn"}
			default:
				result = map[string]any{}
			}
			_ = conn.Write(ctx, websocket.MessageText, mustJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result":  result,
			}))
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	port := ln.Addr().(*net.TCPAddr).Port
	u := "ws://127.0.0.1:" + strconv.Itoa(port) + "/ws?server-key=test"
	return u, func() {
		// Cleanup bound: Shutdown must not hang teardown. Expiry here
		// cannot fail an assertion (🎯T31).
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = ln.Close()
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestDialGrokServeHermeticSession(t *testing.T) {
	url, shutdown := startFakeACPServe(t)
	defer shutdown()

	// Fake PID that we claim is alive.
	old := processAlive
	processAlive = func(pid int) bool { return pid == 4242 }
	defer func() { processAlive = old }()

	c, err := dialGrokServe(url, 4242, false, nil, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if err := c.initialize(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := c.openSession(t.TempDir(), "", false, nil); err != nil {
		t.Fatalf("openSession: %v", err)
	}
	if c.SessionID() == "" {
		t.Fatal("empty session id")
	}
	if c.ConnectURL() != url {
		t.Fatalf("url = %q", c.ConnectURL())
	}
	if c.ConnectPID() != 4242 {
		t.Fatalf("pid = %d", c.ConnectPID())
	}
}

func TestConnectModeReattachLoad(t *testing.T) {
	url, shutdown := startFakeACPServe(t)
	defer shutdown()

	old := processAlive
	processAlive = func(pid int) bool { return pid == 99 }
	defer func() { processAlive = old }()

	// First client: new session
	c1, err := dialGrokServe(url, 99, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.initialize(); err != nil {
		t.Fatal(err)
	}
	if err := c1.openSession(t.TempDir(), "", false, nil); err != nil {
		t.Fatal(err)
	}
	sid := c1.SessionID()
	// Disconnect client without killing "serve" (ownsProcess=false).
	c1.Close()

	// Second client: load same session (simulates consumer restart).
	c2, err := dialGrokServe(url, 99, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.initialize(); err != nil {
		t.Fatal(err)
	}
	if err := c2.openSession(t.TempDir(), sid, true, nil); err != nil {
		t.Fatalf("load: %v", err)
	}
	if c2.SessionID() != sid {
		t.Fatalf("session = %q want %q", c2.SessionID(), sid)
	}
}

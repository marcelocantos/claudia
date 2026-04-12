// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marcelocantos/claudia/internal/daemonproto"
)

// startTestServer spins up a Server against a short-path socket plus
// a temp projects dir. The socket lives under /tmp/claudiatest-* so
// we don't bump into macOS's ~104-char sun_path limit (t.TempDir
// returns paths under /var/folders/... that are already 80+ chars).
// It overrides pidAlive so the test doesn't depend on real OS state.
func startTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "cdt-")
	if err != nil {
		t.Fatalf("mkdir temp sock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	projects := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	srv, err := NewServer(Config{
		SocketPath:    sock,
		ProjectsDir:   projects,
		PendingTTL:    500 * time.Millisecond,
		SweepInterval: time.Hour, // disable by making it long
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// Every PID is alive for this test.
	srv.State().pidAliveFn = func(int) bool { return true }

	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	return srv, sock
}

// dialTestClient opens a WS connection to the given UDS.
func dialTestClient(t *testing.T, sock string) *websocket.Conn {
	t.Helper()
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "http://claudiad/rpc", &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

var testIDCounter atomic.Int64

func call(t *testing.T, conn *websocket.Conn, method string, params any, result any) *daemonproto.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	id := testIDCounter.Add(1)
	idRaw, _ := json.Marshal(id)
	pb, _ := json.Marshal(params)
	req := daemonproto.Request{
		JSONRPC:         "2.0",
		ClaudiadVersion: daemonproto.Version,
		ID:              idRaw,
		Method:          method,
		Params:          pb,
	}
	data, _ := json.Marshal(req)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp daemonproto.Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.ID) == 0 {
			continue // notification — ignore for request/response path
		}
		if resp.Error != nil {
			t.Fatalf("%s rpc error: %d %s", method, resp.Error.Code, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
		}
		return &resp
	}
}

func TestServerRegisterAndLookup(t *testing.T) {
	_, sock := startTestServer(t)
	conn := dialTestClient(t, sock)

	call(t, conn, daemonproto.MethodSessionRegister,
		daemonproto.SessionRegisterParams{Cwd: "/tmp/work", SessionID: "sid-a", PID: 1234},
		&daemonproto.SessionRegisterResult{})

	var lookup daemonproto.SessionLookupChainResult
	call(t, conn, daemonproto.MethodSessionLookupChain,
		daemonproto.SessionLookupChainParams{SessionID: "sid-a"}, &lookup)
	if len(lookup.Chain) != 1 || lookup.Chain[0] != "sid-a" {
		t.Fatalf("expected chain [sid-a], got %v", lookup.Chain)
	}
	if lookup.Confidence != daemonproto.ConfidenceDeterministic {
		t.Fatalf("expected deterministic confidence, got %q", lookup.Confidence)
	}
}

func TestServerChainRolloverViaJSONLDrop(t *testing.T) {
	srv, sock := startTestServer(t)
	conn := dialTestClient(t, sock)

	cwd := "/tmp/rollover"
	escaped := EscapeCwd(cwd)
	projectDir := filepath.Join(srv.State().projectsDir, escaped)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	// Subscribe before registering so we catch the chain.update.
	var subResult daemonproto.SessionSubscribeResult
	call(t, conn, daemonproto.MethodSessionSubscribeChainUpdates, struct{}{}, &subResult)

	call(t, conn, daemonproto.MethodSessionRegister,
		daemonproto.SessionRegisterParams{Cwd: cwd, SessionID: "sid-orig", PID: 4242},
		&daemonproto.SessionRegisterResult{})

	// Drop a new jsonl file — simulates /clear rollover.
	time.Sleep(100 * time.Millisecond) // let fsnotify settle its watch
	newPath := filepath.Join(projectDir, "sid-rollover.jsonl")
	if err := os.WriteFile(newPath, []byte(`{"type":"system"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	// Wait for the chain.update notification to arrive.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gotNotification := false
	for !gotNotification {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp daemonproto.Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Method != daemonproto.NotificationChainUpdate {
			continue
		}
		var params daemonproto.ChainUpdateParams
		if err := json.Unmarshal(resp.Params, &params); err != nil {
			t.Fatalf("unmarshal update params: %v", err)
		}
		if len(params.Chain) == 2 && params.Chain[0] == "sid-orig" && params.Chain[1] == "sid-rollover" {
			gotNotification = true
		}
	}

	// LookupChain should now return the full chain.
	var lookup daemonproto.SessionLookupChainResult
	call(t, conn, daemonproto.MethodSessionLookupChain,
		daemonproto.SessionLookupChainParams{SessionID: "sid-orig"}, &lookup)
	if len(lookup.Chain) != 2 || lookup.Chain[1] != "sid-rollover" {
		t.Fatalf("expected 2-element chain ending sid-rollover, got %v", lookup.Chain)
	}
}

func TestServerSubscriptionCleanupOnDisconnect(t *testing.T) {
	srv, sock := startTestServer(t)

	// Open a connection, subscribe, then drop it without calling
	// unsubscribe. Confirm the server reaps the subscription.
	conn := dialTestClient(t, sock)

	var sub daemonproto.SessionSubscribeResult
	call(t, conn, daemonproto.MethodSessionSubscribeChainUpdates, struct{}{}, &sub)

	// One subscriber currently registered.
	srv.State().mu.Lock()
	initialCount := len(srv.State().subscribers)
	srv.State().mu.Unlock()
	if initialCount != 1 {
		t.Fatalf("expected 1 subscriber after subscribe, got %d", initialCount)
	}

	// Hard close.
	_ = conn.Close(websocket.StatusGoingAway, "")

	// Poll until the server has dropped the subscription.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.State().mu.Lock()
		count := len(srv.State().subscribers)
		srv.State().mu.Unlock()
		if count == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("subscriber was not reaped within deadline")
}

func TestServerUnknownMethodReturnsError(t *testing.T) {
	_, sock := startTestServer(t)
	conn := dialTestClient(t, sock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	id := testIDCounter.Add(1)
	idRaw, _ := json.Marshal(id)
	req := daemonproto.Request{
		JSONRPC:         "2.0",
		ClaudiadVersion: daemonproto.Version,
		ID:              idRaw,
		Method:          "session.nonsense",
	}
	data, _ := json.Marshal(req)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp daemonproto.Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.ID) == 0 {
			continue
		}
		if resp.Error == nil || resp.Error.Code != daemonproto.ErrorCodeMethodNotFound {
			t.Fatalf("expected method-not-found error, got %+v", resp.Error)
		}
		return
	}
}

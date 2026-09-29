// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// jevons 🎯T928: Claude Code opens every MCP connection with a pre-initialize
// server/discover. The mcpbridge-fronted servers never answer it, and the
// host used to hold the backend lock on a read with no deadline, so one
// unanswered request left that server dead for every seat until the broker
// restarted. The fixture drops unknown and pre-initialize requests the same
// way.

type mcpReply struct {
	status  int
	body    map[string]json.RawMessage
	elapsed time.Duration
}

func postMCP(t *testing.T, url, body string, wait time.Duration) mcpReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v (after %s)", body, err, time.Since(start))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := mcpReply{status: resp.StatusCode, elapsed: time.Since(start)}
	if err := json.Unmarshal(raw, &out.body); err != nil {
		t.Fatalf("POST %s: body %q: %v", body, raw, err)
	}
	return out
}

func fixtureURL(t *testing.T) string {
	t.Helper()
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	return h.Attach([]MCPServer{{Name: "fixture", Command: bin}})[0].URL
}

const fixtureInit = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

func errorCode(t *testing.T, r mcpReply) int {
	t.Helper()
	var e struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(r.body["error"], &e); err != nil {
		t.Fatalf("no JSON-RPC error in %v: %v", r.body, err)
	}
	return e.Code
}

func TestMCPHostStdioPreInitializeDiscoverAnsweredByHost(t *testing.T) {
	url := fixtureURL(t)
	r := postMCP(t, url, `{"jsonrpc":"2.0","id":"server-discover-probe-1","method":"server/discover","params":{}}`, 5*time.Second)
	if r.status != http.StatusOK || errorCode(t, r) != -32601 {
		t.Fatalf("discover: status %d body %v, want 200 with -32601", r.status, r.body)
	}
	if got := string(r.body["id"]); got != `"server-discover-probe-1"` {
		t.Fatalf("discover id = %s", got)
	}
	init := postMCP(t, url, fixtureInit, 5*time.Second)
	if init.status != http.StatusOK || init.body["result"] == nil {
		t.Fatalf("initialize after discover: status %d body %v", init.status, init.body)
	}
}

func TestMCPHostStdioUnansweredRequestFailsAtItsDeadlineAndFreesTheBackend(t *testing.T) {
	prev := mcpStdioRequestTimeout
	mcpStdioRequestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mcpStdioRequestTimeout = prev })
	url := fixtureURL(t)
	if r := postMCP(t, url, fixtureInit, 5*time.Second); r.body["result"] == nil {
		t.Fatalf("initialize: %v", r.body)
	}
	dropped := postMCP(t, url, `{"jsonrpc":"2.0","id":7,"method":"nobody/answers"}`, 5*time.Second)
	if dropped.status != http.StatusOK || errorCode(t, dropped) != mcpStdioTimeoutCode || string(dropped.body["id"]) != "7" {
		t.Fatalf("unanswered: status %d body %v", dropped.status, dropped.body)
	}
	if dropped.elapsed > 3*time.Second {
		t.Fatalf("unanswered request took %s, deadline was %s", dropped.elapsed, mcpStdioRequestTimeout)
	}
	ping := postMCP(t, url, `{"jsonrpc":"2.0","id":8,"method":"ping"}`, 5*time.Second)
	if ping.status != http.StatusOK || ping.body["result"] == nil {
		t.Fatalf("ping after an unanswered request: status %d body %v", ping.status, ping.body)
	}
}

// Two seats share one process and both number their requests from the same
// small integers. A slow request must not hold up a fast one, and each seat
// must get its own reply back under its own id.
func TestMCPHostStdioConcurrentSeatsWithTheSameIDGetTheirOwnReplies(t *testing.T) {
	url := fixtureURL(t)
	if r := postMCP(t, url, fixtureInit, 5*time.Second); r.body["result"] == nil {
		t.Fatalf("initialize: %v", r.body)
	}
	slow := make(chan mcpReply, 1)
	go func() {
		slow <- postMCP(t, url, `{"jsonrpc":"2.0","id":1,"method":"sleep","params":{"ms":1500}}`, 10*time.Second)
	}()
	time.Sleep(100 * time.Millisecond) // let the slow request reach the process first
	fast := postMCP(t, url, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, 10*time.Second)
	if string(fast.body["id"]) != "1" || fast.body["result"] == nil || bytes.Contains(fast.body["result"], []byte("slept")) {
		t.Fatalf("fast seat got %v", fast.body)
	}
	if fast.elapsed > time.Second {
		t.Fatalf("ping waited %s behind the slow request", fast.elapsed)
	}
	s := <-slow
	if string(s.body["id"]) != "1" || !bytes.Contains(s.body["result"], []byte("slept")) {
		t.Fatalf("slow seat got %v", s.body)
	}
}

// A seat answering a server-to-client request posts a response, which gets
// no reply; the host must not wait for one.
func TestMCPHostStdioClientResponseIsNotAwaited(t *testing.T) {
	url := fixtureURL(t)
	if r := postMCP(t, url, fixtureInit, 5*time.Second); r.body["result"] == nil {
		t.Fatalf("initialize: %v", r.body)
	}
	r := postMCP(t, url, `{"jsonrpc":"2.0","id":"srv-1","result":{"roots":[]}}`, 5*time.Second)
	if r.status != http.StatusOK || r.elapsed > time.Second {
		t.Fatalf("client response: status %d after %s", r.status, r.elapsed)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// postMCP sends one JSON-RPC body to url and decodes the reply's id and
// tool names. A transport error or non-200 is returned as err.
func postMCP(ctx context.Context, url, body string) (id json.RawMessage, tools []string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &mcpStatusError{resp.StatusCode}
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, nil, err
	}
	for _, t := range msg.Result.Tools {
		tools = append(tools, t.Name)
	}
	return msg.ID, tools, nil
}

type mcpStatusError struct{ code int }

func (e *mcpStatusError) Error() string { return http.StatusText(e.code) }

// 🎯T928: omp's bare tools/list is often the first request a freshly
// started stdio server sees. mcpbridge queues it until initialize; the host
// used to hold its lock waiting for that reply, so the initialize that
// would release it was never written and the server hung for every seat.
func TestMCPHostStdioToolsListBeforeAnyInitialize(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	url := h.Attach([]MCPServer{{Name: "fixture", Command: bin}})[0].URL

	ctx := wallclockguard.UntilTestTimeout(t)
	id, tools, err := postMCP(ctx, url, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if err != nil {
		t.Fatalf("tools/list as first request: %v", err)
	}
	if string(id) != "1" || len(tools) != 1 || tools[0] != "fixture_tool" {
		t.Fatalf("tools/list reply id=%s tools=%v", id, tools)
	}
	// A seat's own initialize still reaches the server afterwards.
	if _, _, err := postMCP(ctx, url, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"seat","version":"0"}}}`); err != nil {
		t.Fatalf("seat initialize after tools/list: %v", err)
	}
}

// 🎯T928: a request the server never answers fails at its own deadline and
// holds up nothing else; seats that number their requests alike each get
// their own reply under their own id.
func TestMCPHostStdioUnansweredRequestBlocksNoOne(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	url := h.Attach([]MCPServer{{Name: "fixture", Command: bin}})[0].URL

	ctx := wallclockguard.UntilTestTimeout(t)
	neverCtx, neverCancel := context.WithCancel(ctx)
	defer neverCancel()
	neverDone := make(chan error, 1)
	go func() {
		_, _, err := postMCP(neverCtx, url, `{"jsonrpc":"2.0","id":1,"method":"never"}`)
		neverDone <- err
	}()
	// "never" is in flight before the seats ask: the backend is waiting on
	// a reply that will not come.
	for !stdioHasPending(h, "fixture") {
		time.Sleep(time.Millisecond)
	}

	const seats = 5
	var wg sync.WaitGroup
	errs := make(chan string, seats)
	for range seats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, tools, err := postMCP(ctx, url, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if err != nil {
				errs <- err.Error()
				return
			}
			if string(id) != "1" || len(tools) != 1 {
				errs <- "reply id=" + string(id)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("tools/list behind an unanswered request: %s", e)
	}

	neverCancel()
	if err := <-neverDone; err == nil {
		t.Fatal("the never-answered request got a reply")
	}
}

// stdioHasPending reports whether the named stdio backend has a request
// waiting on its process for a reply, after the host's own handshake.
func stdioHasPending(h *MCPHost, name string) bool {
	h.mu.Lock()
	b := h.stdio[name]
	h.mu.Unlock()
	if b == nil {
		return false
	}
	b.mu.Lock()
	p := b.proc
	b.mu.Unlock()
	if p == nil {
		return false
	}
	select {
	case <-p.ready: // past the host's own initialize: pending is a seat's
	default:
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending) > 0
}

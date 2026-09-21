// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMCPCodexSeatCallsToolLive is 🎯T119's live gate. Listing tools (the
// existing mnemo tests) does not cross the Codex approval gate; a tools/call
// does. Codex 0.155 rejects every MCP call under approvalPolicy "never"
// unless the seat's MCP block carries default_tools_approval_mode = "approve"
// (5fa7f35). The oracle is the server side: the in-process HTTP MCP server
// must have received a tools/call, and the seat must echo a nonce that only
// the tool result carries.
func TestMCPCodexSeatCallsToolLive(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set")
	}
	if _, err := resolveCodexBin(); err != nil {
		t.Skip(err)
	}

	nonce := "T119-" + strings.ToUpper(uuid.NewString()[:8])
	var calls atomic.Int32
	var callLog []string
	var logMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// No server-initiated stream; the client tolerates 405 on GET.
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "t119probe", "version": "0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "t119_nonce",
				"description": "Returns the verification nonce. Call it with no arguments.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			}}}
		case "tools/call":
			calls.Add(1)
			logMu.Lock()
			callLog = append(callLog, "tools/call "+string(req.Params))
			logMu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": nonce}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()

	agent, err := Start(Config{
		Provider:     ProviderCodex,
		WorkDir:      t.TempDir(),
		MCPServers:   []MCPServer{{Name: "t119probe", Type: "http", URL: srv.URL}},
		MCPExclusive: true,
		TermLogPath:  "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(t.Context()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := agent.Send("Call the MCP tool t119_nonce (server t119probe) once, then reply with exactly the text it returned and nothing else."); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reply, err := agent.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	// Through a broker (CLAUDIA_BROKER_SOCKET, CLAUDIA_NO_BROKER=0) the seat
	// must be the daemon's, or this proves nothing about the broker path.
	if os.Getenv("CLAUDIA_BROKER_SOCKET") != "" && !agent.DaemonHeld() {
		t.Fatal("CLAUDIA_BROKER_SOCKET is set but the seat is not daemon-held")
	}
	logMu.Lock()
	t.Logf("mcp server log: %q daemonHeld=%v", callLog, agent.DaemonHeld())
	logMu.Unlock()
	if calls.Load() == 0 {
		t.Fatalf("server never received tools/call (approval gate rejected it?); reply: %q", reply)
	}
	if !strings.Contains(reply, nonce) {
		t.Fatalf("reply lacks tool-result nonce %q: %q", nonce, reply)
	}
	t.Logf("codex seat called MCP tool: calls=%d reply=%q", calls.Load(), reply)
}

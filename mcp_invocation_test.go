// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

type invocationEvent struct {
	Method, ID, Tool, Session, Seat string
	Identity                        MCPInvocationIdentity
}

// A real independent MCP process remains alive while its simulated executor
// is absent. Releasing the executor completes the original HTTP invocation.
// No provider, broker, authentication or shared state is involved.
func TestMCPInvocationServerProcess(t *testing.T) {
	if os.Getenv("CLAUDIA_MCP_INVOCATION_FIXTURE") != "1" {
		return
	}
	events := make(chan invocationEvent, 64)
	release := make(chan struct{})
	var sessions atomic.Int64
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		select {
		case event := <-events:
			_ = json.NewEncoder(w).Encode(event)
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) { close(release) })
	mux.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(calls.Load()) })
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string                           `json:"name"`
				RequestID string                           `json:"requestId"`
				Meta      map[string]MCPInvocationIdentity `json:"_meta"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("transport-%d", sessions.Add(1)))
			result = map[string]any{"protocolVersion": "2025-03-26"}
		case "tools/list":
			var tools []any
			for _, name := range []string{"durable", "ordinary", "instant", "lost"} {
				tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			calls.Add(1)
			var id string
			_ = json.Unmarshal(request.ID, &id)
			event := invocationEvent{Method: request.Method, ID: id, Tool: request.Params.Name,
				Session: r.Header.Get("Mcp-Session-Id"), Seat: r.Header.Get(jevonsSeatHeader), Identity: request.Params.Meta[MCPInvocationMetaKey]}
			events <- event
			if request.Params.Name == "lost" {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					panic(err)
				}
				_ = conn.Close()
				return
			}
			if request.Params.Name != "instant" {
				select {
				case <-release:
				case <-r.Context().Done():
					event.Method = "disconnected"
					events <- event
					return
				}
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "completed"}}}
		case "notifications/cancelled":
			events <- invocationEvent{Method: request.Method, ID: request.Params.RequestID,
				Session: r.Header.Get("Mcp-Session-Id"), Seat: r.Header.Get(jevonsSeatHeader)}
			w.WriteHeader(http.StatusAccepted)
			return
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(ln) }()
	fmt.Println("http://" + ln.Addr().String())
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = server.Close()
}

func invocationProcess(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestMCPInvocationServerProcess$")
	cmd.Env = append(os.Environ(), "CLAUDIA_MCP_INVOCATION_FIXTURE=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("MCP fixture: %v", err)
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func nextInvocationEvent(t *testing.T, url string) invocationEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var event invocationEvent
	if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
		t.Fatal(err)
	}
	return event
}

func configureInvocationTools(ctrl *ompControl, url string) {
	ctrl.applySessionMCPServers(context.Background(), []MCPServer{{Name: "fixture", Type: "http", URL: url + "/mcp", DurableTools: []string{"durable"}}})
}

func TestMCPInvocationIdentityScopedBySeatAndSession(t *testing.T) {
	url := invocationProcess(t)
	for _, identity := range []MCPInvocationIdentity{{"seat-A", "session-A", "same-call"}, {"seat-B", "session-A", "same-call"}, {"seat-A", "session-B", "same-call"}} {
		ctrl := &ompControl{seat: identity.Seat, sessionID: identity.SessionID}
		configureInvocationTools(ctrl, url)
		// The route can be renamed for a provider; identity and original
		// server tool names must survive that adapter too.
		ctrl.toolServers["renamed"] = url + "/mcp"
		ctrl.toolNames["renamed"] = "instant"
		if result := ctrl.runTool("renamed", identity.ToolCallID, "{}"); result != "completed" {
			t.Fatal(result)
		}
		event := nextInvocationEvent(t, url)
		if event.Identity != identity || event.ID != identity.ToolCallID || event.Seat != identity.Seat || event.Tool != "instant" || event.Session == identity.SessionID {
			t.Fatalf("identity lost or conflated with transport: %+v", event)
		}
	}
}

func TestMCPInvocationRequiresDurableIdentityAndNeverRetriesLostReply(t *testing.T) {
	url := invocationProcess(t)
	ctrl := &ompControl{seat: "seat-A"}
	configureInvocationTools(ctrl, url)
	if got := ctrl.runTool("durable", "call-A", "{}"); !strings.Contains(got, "not sent") {
		t.Fatalf("missing session identity was accepted: %s", got)
	}
	ctrl.sessionID = "session-A"
	if got := ctrl.runTool("durable", "", "{}"); !strings.Contains(got, "not sent") {
		t.Fatalf("missing call identity was accepted: %s", got)
	}
	ctrl.mcpServers[0].DurableTools = append(ctrl.mcpServers[0].DurableTools, "lost")
	if got := ctrl.runTool("lost", "call-A", "{}"); !strings.Contains(got, "outcome unknown") {
		t.Fatalf("transport loss was not an uncertain outcome: %s", got)
	}
	if event := nextInvocationEvent(t, url); event.Tool != "lost" || event.ID != "call-A" {
		t.Fatal(event)
	}
	resp, err := http.Get(url + "/count")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var calls int
	if err := json.NewDecoder(resp.Body).Decode(&calls); err != nil || calls != 1 {
		t.Fatalf("missing identity was sent or lost reply was retried: calls=%d err=%v", calls, err)
	}
}

func TestMCPDurableInvocationOutlivesOrdinaryDeadlineThroughPump(t *testing.T) {
	url := invocationProcess(t)
	ctrl, _, reader, peer, _ := receiptPipe(t)
	configureInvocationTools(ctrl, url)
	_ = peer.SetDeadline(time.Now().Add(2 * mcpToolCallTimeout))
	replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "durable-call", Name: "durable", Text: "{}", TurnID: "turn-A"})
	durable := nextInvocationEvent(t, url)
	if durable.ID != "durable-call" {
		t.Fatal(durable)
	}
	replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "ordinary-call", Name: "ordinary", Text: "{}", TurnID: "turn-A"})
	if event := nextInvocationEvent(t, url); event.ID != "ordinary-call" {
		t.Fatal(event)
	}
	// The unchanged real ordinary deadline is the clock: its failure occurs
	// after the earlier durable call would also have expired under old code.
	ordinary := nextReceiptMessage(t, reader)
	if ordinary.CallID != "ordinary-call" || !strings.Contains(ordinary.Result, "deadline exceeded") || !strings.Contains(ordinary.Result, "outcome unknown") {
		t.Fatalf("ordinary timeout or durable isolation changed: %+v", ordinary)
	}
	resp, err := http.Post(url+"/release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	completed := nextReceiptMessage(t, reader)
	if completed.CallID != durable.ID || completed.Result != "completed" {
		t.Fatalf("original durable invocation did not complete: %+v", completed)
	}
}

func TestMCPInvocationCancellationThroughProductionControl(t *testing.T) {
	for _, action := range []string{"abort", "drop", "turn-end", "pump-close"} {
		t.Run(action, func(t *testing.T) {
			url := invocationProcess(t)
			ctrl, _, reader, peer, _ := receiptPipe(t)
			configureInvocationTools(ctrl, url)
			replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "cancel-me", Name: "durable", Text: "{}", TurnID: "turn-A"})
			started := nextInvocationEvent(t, url)
			if action == "turn-end" {
				// A delayed abort for an older turn must not cancel a new
				// turn's tool on this same seat and connection.
				replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "keep-me", Name: "durable", Text: "{}", TurnID: "turn-B"})
				if event := nextInvocationEvent(t, url); event.ID != "keep-me" {
					t.Fatal(event)
				}
			}
			switch action {
			case "abort", "drop":
				if err := ctrl.send(omp.Message{Op: action, Seat: ctrl.seat, SessionID: ctrl.sessionID}); err != nil {
					t.Fatal(err)
				}
				if got := nextReceiptMessage(t, reader); got.Op != action {
					t.Fatal(got)
				}
			case "turn-end":
				replyReceipt(t, peer, omp.Event{Type: "turn_end", TurnID: "turn-A", StopReason: ReasonAborted})
			case "pump-close":
				_ = peer.Close()
			}
			var disconnected, cancelled bool
			for range 2 {
				event := nextInvocationEvent(t, url)
				if event.ID != started.ID || event.Session != started.Session || event.Seat != started.Seat {
					t.Fatalf("cancellation identity changed: %+v / %+v", started, event)
				}
				disconnected = disconnected || event.Method == "disconnected"
				cancelled = cancelled || event.Method == "notifications/cancelled"
			}
			if !disconnected || !cancelled {
				t.Fatalf("cancellation did not reach both HTTP and MCP: disconnected=%v cancelled=%v", disconnected, cancelled)
			}
			if action == "turn-end" {
				resp, err := http.Post(url+"/release", "application/json", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
				var kept bool
				for range 2 {
					result := nextReceiptMessage(t, reader)
					if result.CallID == "keep-me" {
						kept = result.Result == "completed"
					}
				}
				if !kept {
					t.Fatal("older turn cancellation crossed into the new turn")
				}
			}
		})
	}
}

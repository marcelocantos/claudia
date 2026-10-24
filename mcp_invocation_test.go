// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

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
			if os.Getenv("CLAUDIA_MCP_STATELESS_FIXTURE") != "1" {
				w.Header().Set("Mcp-Session-Id", fmt.Sprintf("transport-%d", sessions.Add(1)))
			}
			result = map[string]any{"protocolVersion": "2025-03-26"}
		case "tools/list":
			var tools []any
			for _, name := range []string{"durable", "ordinary", "instant", "lost", "stream-ok", "stream-error", "stream-progress", "stream-partial", "stream-wrong", "stream-interrupted", "stream-oversize", "json-oversize"} {
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
			if strings.HasPrefix(request.Params.Name, "stream-") {
				w.Header().Set("Content-Type", "text/event-stream")
				progress := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n"
				_, _ = io.WriteString(w, progress)
				terminal := map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "completed"}}}}
				switch request.Params.Name {
				case "stream-progress":
					return
				case "stream-partial":
					_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\"")
					return
				case "stream-wrong":
					terminal["id"] = "another-call"
				case "stream-error":
					delete(terminal, "result")
					terminal["error"] = map[string]any{"code": -32603, "message": "fixture refusal"}
				case "stream-interrupted":
					w.(http.Flusher).Flush()
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						panic(err)
					}
					_ = conn.Close()
					return
				case "stream-oversize":
					_, _ = io.WriteString(w, strings.Repeat(": padding\n\n", mcpReplyLimit/10+1))
				}
				raw, _ := json.Marshal(terminal)
				_, _ = fmt.Fprintf(w, "data: %s\n\n%s", raw, progress)
				return
			}
			if request.Params.Name == "json-oversize" {
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", mcpReplyLimit)}}}})
				return
			}
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
				Session: r.Header.Get("Mcp-Session-Id"), Seat: r.Header.Get(jevonsSeatHeader), Identity: request.Params.Meta[MCPInvocationMetaKey]}
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

func invocationProcess(t *testing.T, stateless bool) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestMCPInvocationServerProcess$")
	cmd.Env = append(os.Environ(), "CLAUDIA_MCP_INVOCATION_FIXTURE=1")
	if stateless {
		cmd.Env = append(cmd.Env, "CLAUDIA_MCP_STATELESS_FIXTURE=1")
	}
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
	ctx := t.Context()
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
	url := invocationProcess(t, false)
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
	url := invocationProcess(t, false)
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
	url := invocationProcess(t, false)
	ctrl, _, reader, peer, _ := receiptPipe(t)
	configureInvocationTools(ctrl, url)
	replyReceipt(t, peer, omp.Event{Type: "accepted", TurnID: "turn-A"})
	replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "durable-call", Name: "durable", Text: "{}"})
	durable := nextInvocationEvent(t, url)
	if durable.ID != "durable-call" {
		t.Fatal(durable)
	}
	replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "ordinary-call", Name: "ordinary", Text: "{}"})
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
			url := invocationProcess(t, false)
			ctrl, _, reader, peer, _ := receiptPipe(t)
			configureInvocationTools(ctrl, url)
			replyReceipt(t, peer, omp.Event{Type: "accepted", TurnID: "turn-A"})
			replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "cancel-me", Name: "durable", Text: "{}"})
			started := nextInvocationEvent(t, url)
			if action == "turn-end" {
				// A delayed abort for an older turn must not cancel a new
				// turn's tool on this same seat and connection.
				receipt := testReceipt("request-B")
				receipt.State, receipt.TurnIDs = ReceiptStarted, []string{"turn-B"}
				replyReceipt(t, peer, omp.Event{Type: "receipt", TurnID: "turn-B", RequestID: "request-B", Receipt: receipt})
				replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "keep-me", Name: "durable", Text: "{}"})
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

func TestMCPDropRefusesBufferedToolAdmission(t *testing.T) {
	url := invocationProcess(t, false)
	ctrl, _, reader, peer, _ := receiptPipe(t)
	configureInvocationTools(ctrl, url)
	if err := ctrl.send(omp.Message{Op: omp.OpDrop, Seat: ctrl.seat, SessionID: ctrl.sessionID}); err != nil {
		t.Fatal(err)
	}
	if msg := nextReceiptMessage(t, reader); msg.Op != omp.OpDrop {
		t.Fatal(msg)
	}
	// This is already buffered on the peer after the drop. It may reach the
	// pump, but it must never reach the MCP executor.
	replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "too-late", Name: "instant", Text: "{}"})
	if msg := nextReceiptMessage(t, reader); msg.CallID != "too-late" || !strings.Contains(msg.Result, "not sent") {
		t.Fatal(msg)
	}
	resp, err := http.Get(url + "/count")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var count int
	if err := json.NewDecoder(resp.Body).Decode(&count); err != nil || count != 0 {
		t.Fatalf("drop admitted tool: count=%d err=%v", count, err)
	}
}

func TestMCPAbortCancelsBeforeBlockedControlWrite(t *testing.T) {
	url := invocationProcess(t, false)
	ctrl, _, _, peer, _ := receiptPipe(t)
	configureInvocationTools(ctrl, url)
	replyReceipt(t, peer, omp.Event{Type: "accepted", TurnID: "turn-A"})
	replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "cancel-me", Name: "durable", Text: "{}"})
	started := nextInvocationEvent(t, url)
	blocked := make(chan error, 1)
	go func() { blocked <- ctrl.send(omp.Message{Op: omp.OpPrompt, Text: strings.Repeat("x", 16<<20)}) }()
	// Observe that the real socket writer has started, then stop reading.
	// The frame cannot fit in the socket buffer. No test deadline races it.
	var first [1]byte
	if _, err := io.ReadFull(peer, first[:]); err != nil {
		t.Fatal(err)
	}
	aborted := make(chan error, 1)
	go func() { aborted <- ctrl.send(omp.Message{Op: omp.OpAbort, Seat: ctrl.seat}) }()
	for range 2 {
		event := nextInvocationEvent(t, url)
		if event.ID != started.ID || (event.Method != "disconnected" && event.Method != "notifications/cancelled") {
			t.Fatal(event)
		}
	}
	// The real production bound, not a fixture clock, releases the writer.
	if err := <-blocked; !errors.Is(err, context.DeadlineExceeded) && !os.IsTimeout(err) {
		t.Fatalf("unread control write: %v", err)
	}
	if err := <-aborted; err == nil {
		t.Fatal("abort unexpectedly reached nonreading peer")
	}
}

func TestMCPStatelessCancellationKeepsLogicalSessionIdentity(t *testing.T) {
	url := invocationProcess(t, true)
	first, _, firstReader, firstPeer, _ := receiptPipe(t)
	second, _, secondReader, secondPeer, _ := receiptPipe(t)
	second.mu.Lock()
	second.sessionID = "another-session"
	second.mu.Unlock()
	configureInvocationTools(first, url)
	configureInvocationTools(second, url)
	for _, peer := range []net.Conn{firstPeer, secondPeer} {
		replyReceipt(t, peer, omp.Event{Type: "accepted", TurnID: "turn-A"})
		replyReceipt(t, peer, omp.Event{Type: "tool_call", CallID: "same-call", Name: "durable", Text: "{}"})
		_ = nextInvocationEvent(t, url)
	}
	if err := first.send(omp.Message{Op: omp.OpAbort, Seat: first.seat}); err != nil {
		t.Fatal(err)
	}
	if msg := nextReceiptMessage(t, firstReader); msg.Op != omp.OpAbort {
		t.Fatal(msg)
	}
	want := MCPInvocationIdentity{Seat: first.seat, SessionID: first.sessionID, ToolCallID: "same-call"}
	for range 2 {
		event := nextInvocationEvent(t, url)
		if event.Identity != want || event.Session != "" {
			t.Fatalf("stateless cancellation conflated sessions: %+v", event)
		}
	}
	resp, err := http.Post(url+"/release", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if msg := nextReceiptMessage(t, secondReader); msg.CallID != "same-call" || msg.Result != "completed" {
		t.Fatalf("other session lost its call: %+v", msg)
	}
}

func TestMCPDurableRequiresMatchingTerminalResponse(t *testing.T) {
	url := invocationProcess(t, false)
	ctrl := &ompControl{seat: "seat", sessionID: "session"}
	configureInvocationTools(ctrl, url)
	for _, name := range []string{"stream-ok", "stream-error", "stream-progress", "stream-partial", "stream-wrong", "stream-interrupted", "stream-oversize", "json-oversize"} {
		t.Run(name, func(t *testing.T) {
			ctrl.mcpServers[0].DurableTools = []string{name}
			result := ctrl.runTool(name, "original-call", "{}")
			want := "outcome unknown"
			if name == "stream-ok" {
				want = "completed"
			}
			if name == "stream-error" {
				want = "fixture refusal"
			}
			if !strings.Contains(result, want) {
				t.Fatalf("%s response incorrectly completed: %s", name, result)
			}
			event := nextInvocationEvent(t, url)
			if event.Tool != name || event.ID != "original-call" {
				t.Fatal(event)
			}
		})
	}
}

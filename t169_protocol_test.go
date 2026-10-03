// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// TestT169ProductionProtocolIntegration is fixture-backed production-protocol
// integration, NOT authenticated-provider or deployed-runtime evidence. The
// model catalog, loopback HTTP/MCP endpoints and credential store/login
// are fixtures. The production server, seat, provider HTTP/SSE transport,
// private socket, recovery response and Go event pump are exercised unchanged.
func TestT169ProductionProtocolIntegration(t *testing.T) {
	for _, scenario := range []string{"success", "unchanged", "second-refusal", "prior-text", "builtin-tool", "host-tool", "cancel", "queued"} {
		t.Run(scenario, func(t *testing.T) { t169ProtocolJourney(t, scenario) })
	}
}

type t169Request struct {
	token    string
	messages json.RawMessage
}

func t169ProtocolJourney(t *testing.T, scenario string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("fixture integration requires bun")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var requests []t169Request
	var hostEffects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path == "/mcp" {
			var rpc struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &rpc)
			w.Header().Set("Content-Type", "application/json")
			switch rpc.Method {
			case "initialize":
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"fixture_tool","description":"disposable effect","inputSchema":{"type":"object"}}]}}`)
			case "tools/call":
				hostEffects.Add(1)
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"fixture effect"}]}}`)
			default:
				t.Errorf("unexpected fixture MCP method: %s", rpc.Method)
			}
			return
		}
		var body struct {
			Messages json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		token := r.Header.Get("X-Api-Key")
		if token == "" {
			token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		mu.Lock()
		requests = append(requests, t169Request{token, append(json.RawMessage(nil), body.Messages...)})
		n := len(requests)
		mu.Unlock()
		if scenario == "prior-text" && n == 1 {
			t169SSE(w, "text-error")
			return
		}
		if (scenario == "builtin-tool" || scenario == "host-tool") && n == 1 {
			t169SSE(w, scenario)
			return
		}
		refuse := n == 1 || scenario == "second-refusal" || scenario == "builtin-tool" || scenario == "host-tool"
		if refuse {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"401 OAuth access token has expired"}}`)
			return
		}
		t169SSE(w, "success")
	}))
	defer server.Close()

	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	// The socket and persisted seats share a short private directory.
	socketDir, err := os.MkdirTemp(os.TempDir(), "t169-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	sock := filepath.Join(socketDir, "s.sock")
	script, _ := filepath.Abs("sidecar/server.ts")
	preload, _ := filepath.Abs("sidecar/testdata/t169-provider-preload.ts")
	cmd := exec.CommandContext(ctx, bun, "--preload", preload, script, sock, "--lifeline=stdin")
	cmd.Env = append(omp.ScrubEnv(os.Environ()), "T169_PROVIDER_URL="+server.URL, "JEVONS_SPOOL_DIR="+filepath.Join(dir, "spool"))
	var childLog bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childLog, &childLog
	life, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = life.Close()
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(childLog.String())
		}
	}()
	for !omp.Listening(ctx, sock) {
		select {
		case <-ctx.Done():
			t.Fatal("disposable sidecar did not listen")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Setenv("CLAUDIA_OMP_SOCKET", sock)
	var refreshes atomic.Int32
	replacement := "fixture-new"
	if scenario == "unchanged" {
		replacement = "fixture-old"
	}
	t141Plan(t, "fixture-old", &refreshes, replacement)
	recovering := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRecovery := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRecovery()
	if scenario == "cancel" || scenario == "queued" {
		login := ompLogin.Run
		ompLogin.Run = func(loginCtx context.Context, name string, args ...string) ([]byte, error) {
			close(recovering)
			select {
			case <-release:
				return login(loginCtx, name, args...)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	a, err := StartDirect(Config{Name: "t169-fixture", Provider: Provider(omp.Anthropic), Model: "t169-fixture", WorkDir: dir, TermLogPath: "-", MCPServers: []MCPServer{{Name: "fixture", Type: "http", URL: server.URL + "/mcp"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	_, wire := a.SubscribeTerminal()
	defer a.UnsubscribeTerminal(wire)
	var text strings.Builder
	var effects atomic.Int32
	terminals := make(chan Event, 16)
	accepted := make(chan struct{}, 16)
	sub := a.SubscribeEvents(func(e Event) {
		mu.Lock()
		if e.Type == "assistant" && e.StopReason == "" {
			text.WriteString(e.Text)
		}
		mu.Unlock()
		if e.ProgressType == "tool_use" {
			effects.Add(1)
		}
		if e.ProgressType == ProgressPromptAccepted {
			accepted <- struct{}{}
		}
		if e.StopReason != "" {
			terminals <- e
		}
	})
	defer a.UnsubscribeEvents(sub)
	if err := a.Send("original T169 prompt"); err != nil {
		t.Fatal(err)
	}
	if scenario == "cancel" || scenario == "queued" {
		select {
		case <-recovering:
		case <-ctx.Done():
			t.Fatal("no recovery handshake")
		}
		if scenario == "queued" {
			if err := a.Send("queued T169 prompt"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				select {
				case <-accepted:
				case <-ctx.Done():
					t.Fatal("queue was not accepted")
				}
			}
		} else if err := a.Interrupt(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		count := len(requests)
		mu.Unlock()
		if count != 1 {
			t.Errorf("input overtook failed attempt resolution: %d requests", count)
		}
		releaseRecovery()
	}
	var end Event
	select {
	case end = <-terminals:
	case <-ctx.Done():
		t.Fatal("no bounded terminal response")
	}
	if scenario == "success" || scenario == "queued" {
		if end.IsError {
			t.Fatalf("intermediate/terminal error: %s", end.Text)
		}
	} else if scenario != "cancel" && !end.IsError {
		t.Fatal("guard lost original refusal")
	}
	if scenario == "cancel" {
		// A subsequent prompt is the barrier proving late recovery cannot replay
		// the cancelled original or authorize a different turn's request.
		if err := a.Send("after cancellation"); err != nil {
			t.Fatal(err)
		}
		select {
		case end = <-terminals:
		case <-ctx.Done():
			t.Fatal("post-cancel turn did not finish")
		}
	}
	mu.Lock()
	finalText := text.String()
	got := append([]t169Request(nil), requests...)
	mu.Unlock()
	if (scenario == "success" || scenario == "queued") && !strings.Contains(finalText, "fixture final") {
		t.Fatal("SSE final text missing")
	}
	if scenario == "builtin-tool" {
		effect, err := os.ReadFile(filepath.Join(dir, "t169-effect"))
		if err != nil || string(effect) != "effect" {
			t.Fatalf("built-in effect missing or repeated: %q %v", effect, err)
		}
	}
	if scenario == "host-tool" && (effects.Load() != 1 || hostEffects.Load() != 1) {
		t.Fatal("host tool callback not observed")
	}
	want := 2
	if scenario == "unchanged" || scenario == "prior-text" {
		want = 1
	}
	if scenario == "queued" {
		want = 3
	}
	if len(got) != want {
		t.Fatalf("requests=%d want %d", len(got), want)
	}
	if got[0].token != "fixture-old" {
		t.Fatal("first request did not use fixture failed credential")
	}
	if scenario == "success" || scenario == "queued" || scenario == "second-refusal" {
		if got[1].token != "fixture-new" {
			t.Fatal("retry did not receive different replacement credential")
		}
		if !bytes.Equal(got[0].messages, got[1].messages) {
			t.Fatalf("retry changed exact messages:\n%s\n%s", got[0].messages, got[1].messages)
		}
	}
	if scenario == "queued" {
		if bytes.Contains(got[1].messages, []byte("queued T169")) || !bytes.Contains(got[2].messages, []byte("queued T169")) {
			t.Fatal("queued input overtook original replay")
		}
	}
	if scenario == "cancel" && bytes.Equal(got[0].messages, got[1].messages) {
		t.Fatal("cancelled prompt replayed")
	}
	var handshakes []omp.Event
	wantHandshake := scenario == "success" || scenario == "queued" || scenario == "second-refusal" || scenario == "unchanged"
	for wantHandshake && len(handshakes) == 0 {
		select {
		case raw := <-wire:
			var e omp.Event
			if json.Unmarshal(raw, &e) == nil && e.Type == "auth_retry" {
				handshakes = append(handshakes, e)
			}
		case <-ctx.Done():
			t.Fatal("no correlated handshake event")
		}
	}
wireDrain:
	for {
		select {
		case raw := <-wire:
			var e omp.Event
			if json.Unmarshal(raw, &e) == nil && e.Type == "auth_retry" {
				handshakes = append(handshakes, e)
			}
		default:
			break wireDrain
		}
	}
	if scenario == "success" || scenario == "queued" || scenario == "second-refusal" || scenario == "unchanged" {
		if len(handshakes) != 1 || handshakes[0].RequestID == "" || handshakes[0].TurnID == "" || handshakes[0].FailedToken != tokenFingerprint("fixture-old") {
			t.Fatalf("missing unique correlated failed-attempt handshake: %+v", handshakes)
		}
	}
	if scenario == "prior-text" || scenario == "builtin-tool" || scenario == "host-tool" {
		if len(handshakes) != 0 {
			t.Fatal("unsafe attempt requested retry")
		}
	}
	select {
	case e := <-terminals:
		t.Fatalf("extra terminal event: %+v", e)
	default:
	}
}

func t169SSE(w http.ResponseWriter, mode string) {
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name, body string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body) }
	event("message_start", `{"type":"message_start","message":{"id":"t169","type":"message","role":"assistant","content":[],"model":"t169-fixture","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`)
	stop := "end_turn"
	if mode == "builtin-tool" || mode == "host-tool" {
		name, args := "Bash", `{"command":"printf effect >> t169-effect"}`
		if mode == "host-tool" {
			name, args = "fixture_tool", `{}`
		}
		event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-t169","name":%q,"input":{}}}`, name))
		encoded, _ := json.Marshal(args)
		event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, encoded))
		stop = "tool_use"
	} else {
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fixture final"}}`)
	}
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	if mode == "text-error" {
		event("error", `{"type":"error","error":{"type":"authentication_error","message":"401 OAuth access token has expired"}}`)
		return
	}
	event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":2}}`, stop))
	event("message_stop", `{"type":"message_stop"}`)
}

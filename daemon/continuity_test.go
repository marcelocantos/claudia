// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

// Consumer loss must not become execution-host loss. This uses the real broker,
// real sidecar and real tools; the provider is the registered in-memory mock
// API. It does not claim survival of a broker/sidecar restart.
func TestBrokerActiveToolSurvivesConsumerDisconnect(t *testing.T) {
	for _, tool := range []string{"Bash", "continuity_external"} {
		for _, finishAbsent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/finishAbsent=%v", tool, finishAbsent), func(t *testing.T) {
				f := newFixture(t)
				root := filepath.Dir(f.sock)
				t.Setenv("HOME", root)
				socket := filepath.Join(root, "omp.sock")
				bun, err := exec.LookPath("bun")
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(bun, "../sidecar/testdata/continuity-provider.ts", socket)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CLAUDIA_OMP_SPOOL_DIR=" + filepath.Join(root, "spool")}
				cmd.Stderr = os.Stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
				waitFor(t, "actual sidecar listening", func() bool { return omp.Listening(t.Context(), socket) })
				if api, err := os.ReadFile(filepath.Join(root, "mock-api-ready")); err != nil || string(api) != "mock\n" {
					t.Fatalf("fixture did not select mock API: %q %v", api, err)
				}
				t.Setenv(omp.SocketEnv, socket)
				t.Setenv("CLAUDIA_OMP_NO_REFRESH", "1")
				omp.ResetKeychainShot()
				t.Cleanup(omp.ResetKeychainShot)
				raw, err := json.Marshal(omp.Item{Records: map[string]omp.Record{omp.Anthropic: {AccessToken: "fixture", Expiry: time.Now().Add(time.Hour)}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := omp.Open(t.Context(), omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) { return raw, nil }}); err != nil {
					t.Fatal(err)
				}

				entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				var invocations, effects atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					var req struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
					}
					if r.URL.Path != "/bash" {
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
							return
						}
						switch req.Method {
						case "initialize":
							_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "continuity", "version": "1"}}})
							return
						case "notifications/initialized":
							w.WriteHeader(http.StatusAccepted)
							return
						case "tools/list":
							_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "continuity_external", "description": "fixture barrier", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}})
							return
						case "tools/call":
						default:
							t.Errorf("unexpected MCP method %q", req.Method)
							return
						}
					}
					if invocations.Add(1) != 1 {
						t.Error("duplicate tool invocation")
						return
					}
					close(entered)
					defer close(returned)
					select {
					case <-release:
					case <-r.Context().Done():
						t.Error("consumer disconnect cancelled execution-host HTTP request")
						return
					}
					effects.Add(1)
					if r.URL.Path == "/bash" {
						_, _ = w.Write([]byte("continuity-tool-result"))
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []any{map[string]string{"type": "text", "text": "continuity-tool-result"}}}})
				}))
				t.Cleanup(upstream.Close)
				t.Cleanup(unblock)
				opts := f.options(nil)
				opts.DisableResume = true
				opts.launchers = &claudia.RegistryLaunchers{Start: claudia.StartDirectContext, Adopt: claudia.Adopt}
				f.bootWith(t, opts)
				def := claudia.GrantDefinition{AgentDef: claudia.AgentDef{Name: "continuity", SessionID: "generation", Provider: claudia.ProviderClaude, Model: "claude-sonnet-5", WorkDir: root, TermLogPath: "-", MCPServers: []claudia.MCPServer{{Name: "continuity", Type: "http", URL: upstream.URL}}}}
				encoded, err := claudia.EncodeGrantDefinition(def)
				if err != nil {
					t.Fatal(err)
				}
				first := dialRaw(t, f.sock)
				if resp := rawCall(t, first, &broker.Request{ID: "grant", Type: broker.TypeGrant, Grant: &broker.GrantRequest{Name: def.Name, Def: encoded}}); resp.Type != broker.TypeGranted {
					t.Fatalf("grant: %+v", resp)
				}
				proc := f.d.reg.Get(def.Name)
				if proc == nil {
					t.Fatal("missing daemon execution host")
				}
				terminal := make(chan claudia.Event, 1)
				sub := proc.SubscribeEvents(func(ev claudia.Event) {
					if ev.Type == "assistant" && ev.Final {
						select {
						case terminal <- ev:
						default:
						}
					}
				})
				t.Cleanup(func() { proc.UnsubscribeEvents(sub) })
				calls := filepath.Join(root, "provider-calls")
				args := map[string]string{}
				if tool == "Bash" {
					args["command"] = fmt.Sprintf("printf 'invoked\\n' >> bash-invocations; curl --fail --silent --show-error '%s/bash' && printf 'effect\\n' >> bash-effects", upstream.URL)
				}
				prompt, err := json.Marshal(map[string]any{"tool": tool, "args": args, "calls": calls})
				if err != nil {
					t.Fatal(err)
				}
				if resp := rawCall(t, first, &broker.Request{ID: "send", Type: broker.TypeSend, Send: &broker.SendRequest{Name: def.Name, Text: "CONTINUITY_FIXTURE " + string(prompt)}}); resp.Type != broker.TypeSent {
					t.Fatalf("send: %+v", resp)
				}
				select {
				case <-entered:
				case ev := <-terminal:
					t.Fatalf("turn ended before tool entered: %+v", ev)
				case <-t.Context().Done():
					t.Fatal("tool never entered")
				}
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
				waitFor(t, "consumer detached", func() bool { return !f.owned(def.Name) })
				select {
				case <-returned:
					t.Fatal("held tool returned on consumer loss")
				default:
				}
				if f.d.reg.Get(def.Name) != proc || !proc.Alive() {
					t.Fatal("consumer loss replaced execution host")
				}
				if finishAbsent {
					unblock()
					waitFor(t, "terminal buffered while absent", func() bool {
						f.d.mu.Lock()
						defer f.d.mu.Unlock()
						for _, raw := range f.d.grants[def.Name].ring {
							ev, err := claudia.DecodeEventWire(raw)
							if err == nil && ev.Type == "assistant" && ev.Final {
								return true
							}
						}
						return false
					})
				}
				second := dialRaw(t, f.sock)
				if err := second.WriteRequest(&broker.Request{ID: "reclaim", Type: broker.TypeGrant, Grant: &broker.GrantRequest{Name: def.Name, Def: encoded, Adopt: true, Fallback: true}}); err != nil {
					t.Fatal(err)
				}
				var granted, completed bool
				var text string
				for !granted || !completed {
					resp, err := second.ReadResponse()
					if err != nil {
						t.Fatal(err)
					}
					if resp.Type == broker.TypeError {
						t.Fatalf("reclaim: %+v", resp.Error)
					}
					if resp.Granted != nil {
						granted = true
						if !resp.Granted.Reclaimed || resp.Granted.SessionID != def.SessionID || resp.Granted.Lagged {
							t.Fatalf("replaced or lagged grant: %+v", resp.Granted)
						}
						if f.d.reg.Get(def.Name) != proc {
							t.Fatal("reclaim replaced execution host")
						}
						unblock()
					}
					if resp.AgentEvent != nil {
						ev, err := claudia.DecodeEventWire(resp.AgentEvent.Event)
						if err != nil {
							t.Fatal(err)
						}
						text += ev.Text
						if ev.Type == "assistant" && ev.Final {
							completed = true
						}
					}
				}
				if !strings.Contains(text, "continuity-complete") {
					t.Fatalf("lost completed tool result: %s", text)
				}
				if invocations.Load() != 1 || effects.Load() != 1 {
					t.Fatalf("invocations=%d effects=%d", invocations.Load(), effects.Load())
				}
				checkFile := func(name, want string) {
					t.Helper()
					raw, err := os.ReadFile(filepath.Join(root, name))
					if err != nil || string(raw) != want {
						t.Fatalf("%s=%q: %v", name, raw, err)
					}
				}
				checkFile("provider-calls", "call\nresult\n")
				if tool == "Bash" {
					checkFile("bash-invocations", "invoked\n")
					checkFile("bash-effects", "effect\n")
				}
			})
		}
	}
}

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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// useOMP selects the sidecar. The four subscription plans and the
// fleet ids grok / claude / codex / cursor all go through it (🎯T866.5).
// Config.OMP is no longer required for cursor.

func useOMP(cfg Config) bool {
	return ompProviderID(cfg.Provider) != ""
}

func ompProviderID(p Provider) string {
	switch p {
	case Provider(omp.Anthropic):
		return omp.Anthropic
	case Provider(omp.OpenAICodex):
		return omp.OpenAICodex
	case ProviderCursor:
		return omp.Cursor
	case Provider(omp.XAIOAuth), ProviderGrok:
		return omp.XAIOAuth
	default:
		return ""
	}
}

func agentBackendFor(cfg Config) agentBackend {
	if useOMP(cfg) {
		return ompAgentBackend{}
	}
	return agentBackendForProvider(cfg.Provider)
}

// ompKeychain is the Keychain command runner. Tests replace it. A nil
// runner refuses rather than reading an API key from the environment.
var ompKeychain omp.Runner

// ompLogin is the pi-ai helper. Tests replace it. A zero value uses bun
// sidecar/auth.ts.
var ompLogin omp.Login

type ompAgentBackend struct{}

func (ompAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{Session: true, Resume: true}
}

func (ompAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	provider := ompProviderID(req.Config.Provider)
	if provider == "" {
		return nil, fmt.Errorf("omp: %s is not a sidecar provider", req.Config.Provider)
	}
	if req.Config.RequireResume {
		if !omp.SeatHasHistory(omp.SpoolDir(), req.Config.Name) {
			return nil, fmt.Errorf("session %s: existing conversation required but no spool records for seat %q under %s — refusing to mint a replacement session",
				req.Config.SessionID, req.Config.Name, omp.SpoolDir())
		}
	}
	socket := os.Getenv(omp.SocketEnv)
	if socket == "" {
		var err error
		socket, err = omp.Ensure(req.Context)
		if err != nil {
			return nil, err
		}
	}
	run := ompKeychain
	if run == nil {
		run = execKeychain
	}
	store := omp.Store{BrokerPath: os.Args[0], Run: run}
	login := ompLogin
	if login.Run == nil {
		login.Run = execBunLogin
	}
	if login.Command == "" {
		login.Command = "bun"
	}
	if login.Script == "" {
		login.Script = sidecarAuthScript()
	}
	token, err := store.Ensure(req.Context, provider, login)
	if err != nil {
		return nil, err
	}
	conn, err := omp.Dial(req.Context, socket)
	if err != nil {
		return nil, err
	}
	if err := conn.Send(omp.Message{
		Op:       omp.OpLoad,
		Seat:     req.Config.Name,
		Provider: provider,
		Model:    req.Config.Model,
		Token:    token,
	}); err != nil {
		conn.Close()
		return nil, err
	}
	ev, err := conn.Recv()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if ev.Type != "ready" {
		conn.Close()
		return nil, fmt.Errorf("omp: sidecar said %q, want ready", ev.Type)
	}
	ctrl := &ompControl{conn: conn, bytes: make(chan []byte, 8), token: token, provider: provider}
	return &agentStart{
		Control: ctrl,
		Ops: agentOps{
			send: func(_ *Agent, text string) error {
				ctrl.inflight.Store(true)
				return ctrl.send(omp.Message{Op: omp.OpPrompt, Seat: req.Config.Name, Text: text})
			},
			steer: func(_ *Agent, text string) (DeliveryOutcome, error) {
				err := ctrl.send(omp.Message{Op: omp.OpSteer, Seat: req.Config.Name, Text: text})
				return DeliveryOutcome{}, err
			},
			interrupt: func(*Agent) error {
				return ctrl.send(omp.Message{Op: omp.OpAbort, Seat: req.Config.Name})
			},
			setModel: func(_ *Agent, model string) error {
				return ctrl.send(omp.Message{
					Op:       omp.OpLoad,
					Seat:     req.Config.Name,
					Provider: provider,
					Model:    model,
					Token:    ctrl.token,
				})
			},
			promptInFlight: func(*Agent) bool { return ctrl.inflight.Load() },
			stop:           func(*Agent) { conn.Close() },
		},
		DetectReady: func(a *Agent) {
			go ctrl.pump(a)
			select {
			case <-a.ready:
			default:
				close(a.ready)
			}
		},
		Cleanup: func() { conn.Close() },
	}, nil
}

type ompControl struct {
	conn     *omp.Conn
	bytes    chan []byte
	token    string
	provider string
	mu       sync.Mutex
	inflight atomic.Bool
}

func (c *ompControl) send(msg omp.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Send(msg)
}

func (c *ompControl) pump(a *Agent) {
	defer close(c.bytes)
	for {
		ev, err := c.conn.Recv()
		if err != nil {
			return
		}
		if raw, err := json.Marshal(ev); err == nil {
			select {
			case c.bytes <- raw:
			default:
			}
		}
		switch ev.Type {
		case "text":
			a.publishEvent(Event{
				Type:          "assistant",
				Text:          ev.Text,
				PreviewUpdate: PreviewUpdateAppend,
			})
		case "tool_call":
			a.publishEvent(Event{
				Type:         "progress",
				ProgressType: "tool_use",
				ToolCallID:   ev.CallID,
				ToolTitle:    ev.Name,
				Text:         ev.Text,
			})
			result := runOMPTool(ev.Name, ev.CallID, ev.Text)
			_ = c.send(omp.Message{Op: omp.OpTool, CallID: ev.CallID, Result: result})
		case "turn_end":
			c.inflight.Store(false)
			a.publishEvent(Event{
				Type:       "assistant",
				Text:       ev.Text,
				StopReason: "end_turn",
			})
		case "error":
			c.inflight.Store(false)
			a.publishEvent(Event{
				Type:       "assistant",
				Text:       ev.Text,
				IsError:    true,
				StopReason: "end_turn",
			})
		}
	}
}

func sidecarAuthScript() string {
	if p := os.Getenv("CLAUDIA_OMP_AUTH"); p != "" {
		return p
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "sidecar/auth.ts"
	}
	return filepath.Join(filepath.Dir(file), "sidecar", "auth.ts")
}

// ompToolExec is the Go callback for jevons_* tool calls. Tests replace it.
var ompToolExec func(name, callID, args string) string

// EnsureOMPSidecar starts the detached Bun sidecar if it is not already
// listening. A jevonsd or broker bounce must not call StopSidecar.
func EnsureOMPSidecar(ctx context.Context) (string, error) {
	return omp.Ensure(ctx)
}

// SetOMPToolExec installs the jevons_* callback the sidecar invokes
// (🎯T865). Production brokers set this to an HTTP tools/call against
// the live jevonsmcp URL.
func SetOMPToolExec(fn func(name, callID, args string) string) {
	ompToolExec = fn
}

func runOMPTool(name, callID, args string) string {
	if ompToolExec != nil {
		return ompToolExec(name, callID, args)
	}
	return DefaultOMPToolExec(name, callID, args)
}

// DefaultOMPToolExec POSTs a JSON-RPC tools/call to the jevons MCP
// endpoint. JEVONS_MCP_URL wins; otherwise the development :13705 path.
func DefaultOMPToolExec(name, callID, args string) string {
	url := strings.TrimSpace(os.Getenv("JEVONS_MCP_URL"))
	if url == "" {
		url = "http://127.0.0.1:13705/mcp"
	}
	return CallJevonsMCP(url, name, args)
}

// CallJevonsMCP is the production jevons_* runner: one HTTP JSON-RPC
// tools/call against the daemon's MCP surface.
func CallJevonsMCP(mcpURL, name, args string) string {
	if !strings.HasPrefix(name, "jevons_") {
		return fmt.Sprintf("omp: refusing non-jevons tool %q", name)
	}
	var arguments any
	if strings.TrimSpace(args) == "" {
		arguments = map[string]any{}
	} else if json.Unmarshal([]byte(args), &arguments) != nil {
		arguments = map[string]any{"text": args}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		return fmt.Sprintf("omp: encode %s: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("omp: %s request: %v", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("omp: %s call failed: %v", name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Sprintf("omp: %s read: %v", name, err)
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return strings.TrimSpace(string(raw))
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	var b strings.Builder
	for _, c := range envelope.Result.Content {
		b.WriteString(c.Text)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return strings.TrimSpace(string(raw))
	}
	return out
}

func execKeychain(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = omp.ScrubEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return stderr.Bytes(), fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return out, nil
}

// execBunLogin runs sidecar/auth.ts through bun from that directory so
// @oh-my-pi/pi-ai resolves. It is not the Keychain runner.
func execBunLogin(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = omp.ScrubEnv(os.Environ())
	if dir := filepath.Dir(sidecarAuthScript()); dir != "." && dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return stderr.Bytes(), fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return out, nil
}

func (c *ompControl) Bytes() <-chan []byte { return c.bytes }
func (c *ompControl) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

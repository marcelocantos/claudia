// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/marcelocantos/claudia/omp"
)

// OMP on Config selects the sidecar for ProviderCursor. The subscription
// ids anthropic, openai-codex, and xai-oauth always use the sidecar.
// Existing grok, claude, and codex seats keep their CLIs.

func useOMP(cfg Config) bool {
	switch cfg.Provider {
	case Provider(omp.Anthropic), Provider(omp.OpenAICodex), Provider(omp.XAIOAuth):
		return true
	case ProviderCursor:
		return cfg.OMP
	default:
		return false
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
	provider := string(req.Config.Provider)
	if provider == string(ProviderCursor) && !req.Config.OMP {
		return nil, fmt.Errorf("omp: cursor seat is not marked for the sidecar")
	}
	socket := os.Getenv("CLAUDIA_OMP_SOCKET")
	if socket == "" {
		return nil, fmt.Errorf("omp: CLAUDIA_OMP_SOCKET is unset; refusing to start a vendor CLI")
	}
	run := ompKeychain
	if run == nil {
		run = execKeychain
	}
	store := omp.Store{BrokerPath: os.Args[0], Run: run}
	login := ompLogin
	if login.Run == nil {
		login.Run = run
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
			result := "jevons tool runner is not attached"
			if ompToolExec != nil {
				result = ompToolExec(ev.Name, ev.CallID, ev.Text)
			}
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

func (c *ompControl) Bytes() <-chan []byte { return c.bytes }
func (c *ompControl) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

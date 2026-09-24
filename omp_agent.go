// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"

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
	token, err := store.AccessToken(req.Context, provider)
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
	bytes := make(chan []byte, 1)
	ctrl := &ompControl{conn: conn, bytes: bytes}
	return &agentStart{
		Control: ctrl,
		Ops: agentOps{
			send: func(_ *Agent, text string) error {
				return conn.Send(omp.Message{Op: omp.OpPrompt, Seat: req.Config.Name, Text: text})
			},
			steer: func(_ *Agent, text string) (DeliveryOutcome, error) {
				err := conn.Send(omp.Message{Op: omp.OpSteer, Seat: req.Config.Name, Text: text})
				return DeliveryOutcome{}, err
			},
			interrupt: func(*Agent) error {
				return conn.Send(omp.Message{Op: omp.OpAbort, Seat: req.Config.Name})
			},
			setModel: func(_ *Agent, model string) error {
				return conn.Send(omp.Message{Op: omp.OpLoad, Seat: req.Config.Name, Provider: provider, Model: model, Token: token})
			},
			stop: func(*Agent) { conn.Close() },
		},
		Cleanup: func() { conn.Close() },
	}, nil
}

type ompControl struct {
	conn  *omp.Conn
	bytes chan []byte
}

func execKeychain(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
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

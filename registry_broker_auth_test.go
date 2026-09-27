// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

type authFailBroker struct{}

func (authFailBroker) HandleRequest(c *broker.ClientConn, req *broker.Request) bool {
	if req.Type != broker.TypeGrant {
		return false
	}
	_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeAgentFailed,
		Msg: "anthropic refresh failed: invalid_grant"})
	return true
}

func (authFailBroker) ConnClosed(*broker.ClientConn) {}

func TestBrokerAuthErrorIsNotOverwrittenByLocalFallback(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "claudia-auth-error-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "broker.sock")
	t.Setenv(broker.SocketPathEnv, socket)
	t.Setenv(broker.NoBrokerEnv, "0")
	listener, err := broker.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := broker.Serve(&broker.ServeArgs{Listener: listener, Handler: authFailBroker{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	localStarts := 0
	reg.SetLaunchers(&RegistryLaunchers{Start: func(context.Context, Config) (*Agent, error) {
		localStarts++
		return nil, errors.New("misleading local Keychain error")
	}})
	if err := reg.Register(AgentDef{Name: "failed-auth", WorkDir: t.TempDir(), Provider: "anthropic", SessionID: "auth-sid"}); err != nil {
		t.Fatal(err)
	}
	_, err = reg.AdoptOrLaunch("failed-auth")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || localStarts != 0 {
		t.Fatalf("broker error=%v local starts=%d", err, localStarts)
	}
}

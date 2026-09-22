// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestJudgeFallsBackFromDaemonThatPredatesJudge: a daemon older than 🎯T127
// answers judge with unknown_type. Such a daemon cannot evaluate the
// request and the caller can, so Ask goes direct instead of failing — which
// is the host's state from the moment this ships until the daemon upgrades.
func TestJudgeFallsBackFromDaemonThatPredatesJudge(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cjb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	seen := make(chan broker.MessageType, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		c := broker.NewConn(nc)
		req, err := c.ReadRequest()
		if err != nil {
			return
		}
		seen <- req.Type
		// What ParseRequest in a pre-judge daemon returns, echoed by
		// replyErr with the envelope id.
		pe := &broker.ProtocolError{Code: broker.CodeUnknownType, Field: "type", Value: string(req.Type), ID: req.ID,
			Msg: `"judge" is not a request type in wire version 1`}
		_ = c.WriteResponse(&broker.Response{ID: req.ID, Type: broker.TypeError, Error: pe.Wire()})
	}()
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")

	f := newJevFixture(t, jevReply{status: 200, body: triageAnswer})
	t.Setenv(judgeEndpointEnv, f.srv.URL)
	t.Setenv(judgeKeyEnv, fixtureKey)
	res, err := NewJudge(JudgeConfig{}).Ask(context.Background(), triageRequest())
	if err != nil {
		t.Fatalf("Ask against a pre-judge daemon: %v", err)
	}
	if got := <-seen; got != broker.TypeJudge {
		t.Fatalf("daemon saw %q, want judge", got)
	}
	if res.Model != "jev-1.13.0" || len(f.requests()) != 1 {
		t.Fatalf("direct fallback: model %q, %d API requests", res.Model, len(f.requests()))
	}
}

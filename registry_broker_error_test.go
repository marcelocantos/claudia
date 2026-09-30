// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestLaunchReturnsBrokerAgentFailedOnce: a daemon that answers the grant
// with agent_failed has given its verdict. Launch returns that error and
// does not grant again. Colossus pimp-smoke (2026-09-26) logged
// "adopt failed; falling back to launch" and a second identical OMP
// refusal because the first agent_failed was treated as a miss.
func TestLaunchReturnsBrokerAgentFailedOnce(t *testing.T) {
	// t.TempDir() under the macOS runner TMPDIR plus this test's name
	// exceeds the 104-byte sun_path limit (bind: invalid argument).
	dir, err := os.MkdirTemp("/tmp", "cba")
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
	var grants atomic.Int32
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer nc.Close()
				c := broker.NewConn(nc)
				req, err := c.ReadRequest()
				if err != nil {
					return
				}
				if req.Type == broker.TypeGrant {
					grants.Add(1)
				}
				pe := &broker.ProtocolError{
					Code: broker.CodeAgentFailed,
					Msg:  `omp: sidecar said "error", want ready`,
					ID:   req.ID,
				}
				_ = c.WriteResponse(&broker.Response{ID: req.ID, Type: broker.TypeError, Error: pe.Wire()})
			}()
		}
	}()
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")

	reg, err := NewRegistry(filepath.Join(dir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name: "pimp-smoke", Provider: ProviderGrok, WorkDir: t.TempDir(),
		SessionID: "sid-smoke", TermLogPath: "-", Parent: "pimp", Purpose: PurposeWork,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = reg.Launch("pimp-smoke")
	if err == nil || !strings.Contains(err.Error(), "agent_failed") || !strings.Contains(err.Error(), `sidecar said "error", want ready`) {
		t.Fatalf("Launch = %v, want the daemon's agent_failed handshake", err)
	}
	if got := grants.Load(); got != 1 {
		t.Fatalf("grant requests = %d, want 1", got)
	}
}

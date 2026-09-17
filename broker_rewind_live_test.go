// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestBrokerDaemonRewindLive is 🎯T75.8's live gate: a real Claude seat held
// by a daemon (in-process, on a temp socket, so it never touches an
// installed one) remembers two codewords, is rewound by one turn through
// the consumer's handle, and afterwards recalls the first and not the
// second. Only a real claude --resume can show the relaunch honours the
// truncated transcript.
func TestBrokerDaemonRewindLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	startLiveDaemon(t)

	a, err := Start(Config{Name: "rewind-live-" + newRunID(), WorkDir: t.TempDir(), Model: "haiku"})
	if err != nil {
		t.Fatalf("Start via daemon: %v", err)
	}
	t.Cleanup(a.Stop)
	if a.brokerGrant == "" {
		t.Fatal("seat is not daemon-held")
	}
	turn := func(prompt string) string {
		t.Helper()
		if err := a.Send(prompt); err != nil {
			t.Fatalf("Send: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		reply, err := a.WaitForResponse(ctx)
		if err != nil {
			t.Fatalf("WaitForResponse: %v\nterminal log %s:\n%s", err, a.TermLogPath(), termLogTail(a.TermLogPath()))
		}
		return reply
	}
	turn("Remember this codeword: ALPHA. Reply with only: ok")
	turn("Remember this codeword too: BRAVO. Reply with only: ok")
	sid := a.SessionID()

	got, err := a.Rewind(1, Config{})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if got != a || a.SessionID() != sid {
		t.Fatalf("rewind changed the handle or session: %p/%p %s → %s", got, a, sid, a.SessionID())
	}
	recall := strings.ToUpper(turn("List every codeword I have asked you to remember, comma-separated. If none, reply NONE."))
	t.Logf("post-rewind recall: %q", recall)
	if !strings.Contains(recall, "ALPHA") {
		t.Error("the relaunched seat lost the surviving turn (ALPHA)")
	}
	if strings.Contains(recall, "BRAVO") {
		t.Error("the relaunched seat resurfaced the rewound turn (BRAVO)")
	}
}

// startLiveDaemon runs a daemon in this process on a temp socket and points
// the consumer API at it, so a live test exercises the daemon path without
// touching an installed daemon.
func startLiveDaemon(t *testing.T) *BrokerDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	d, err := NewBrokerDaemon(BrokerDaemonOptions{
		SocketPath: sock, StateDir: filepath.Join(dir, "state"),
		DisableResume: true, DisableIntel: true, DisableMCPHost: true,
		UsageFetch: func(context.Context) ([]PlanUsage, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	return d
}

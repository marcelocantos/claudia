// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// startLegacyCodexSession drives the legacy `codex app-server` backend directly.
// Since 🎯T178 a Codex Session seat goes through the sidecar, so Start no
// longer reaches this backend; its own tests keep covering it until 🎯T192
// removes it.
func startLegacyCodexSession(cfg Config) (*Agent, error) {
	if cfg.Provider == "" {
		cfg.Provider = ProviderCodex
	}
	return startWithBackend(cfg, codexAgentBackend{})
}

// ompFixture is a fake subscription sidecar: it answers load and adopt with
// ready (echoing the sandbox mode it was asked for, as the real sidecar does
// since 🎯T178) and answers each prompt with one text and a turn_end stamped
// with the prompt's turn (🎯T179).
type ompFixture struct {
	loads   chan omp.Message
	prompts chan omp.Message
	// noSandboxEcho models a sidecar that predates 🎯T178.
	noSandboxEcho atomic.Bool
	reply         string
}

var ompFixtureSeq atomic.Int64

func startOMPFixture(t *testing.T) *ompFixture {
	t.Helper()
	f := &ompFixture{loads: make(chan omp.Message, 32), prompts: make(chan omp.Message, 32), reply: "fixture-ok"}
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("cl-ompfx-%d-%d.sock", os.Getpid(), ompFixtureSeq.Add(1)))
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); os.Remove(socket); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				f.serve(c)
			}()
		}
	}()
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{` +
		`"anthropic":{"refresh_token":"r","access_token":"token","expiry":"` + exp + `"},` +
		`"openai-codex":{"refresh_token":"r","access_token":"token","expiry":"` + exp + `"},` +
		`"xai-oauth":{"refresh_token":"r","access_token":"token","expiry":"` + exp + `"},` +
		`"cursor":{"refresh_token":"r","access_token":"token","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }
	t.Cleanup(func() { ompKeychain = nil; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(omp.SocketEnv, socket)
	return f
}

func (f *ompFixture) serve(c net.Conn) {
	r := bufio.NewReader(c)
	receipts := map[string]*omp.RequestReceipt{}
	write := func(v map[string]any) {
		b, _ := json.Marshal(v)
		_, _ = c.Write(append(b, '\n'))
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg omp.Message
		if json.Unmarshal(line, &msg) != nil {
			return
		}
		switch msg.Op {
		case omp.OpLoad, omp.OpAdopt:
			select {
			case f.loads <- msg:
			default:
			}
			ready := map[string]any{"seat": msg.Seat, "type": "ready", "how": "launched"}
			if msg.Op == omp.OpAdopt {
				ready["how"] = "adopted"
			}
			var tools []struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(msg.Tools, &tools)
			names := []string{"Bash", "Read", "Write", "Glob", "Grep"}
			for _, tl := range tools {
				names = append(names, tl.Name)
			}
			ready["bound_tools"] = names
			if msg.Sandbox != nil && !f.noSandboxEcho.Load() {
				ready["sandbox"] = msg.Sandbox.Mode
			}
			write(ready)
		case omp.OpPrompt:
			select {
			case f.prompts <- msg:
			default:
			}
			if msg.RequestID != "" {
				now := time.Now().UTC().Format(time.RFC3339Nano)
				receipt := &omp.RequestReceipt{Seat: msg.Seat, SessionID: msg.SessionID, RequestID: msg.RequestID, AttemptID: msg.AttemptID, State: omp.ReceiptAccepted, TurnIDs: []string{}, AcceptedAt: now, UpdatedAt: now}
				receipts[msg.RequestID] = receipt
				write(map[string]any{"seat": msg.Seat, "type": "accepted", "request_id": msg.RequestID, "rpc_id": msg.RPCID, "receipt": receipt})
			}
			write(map[string]any{"seat": msg.Seat, "type": "text", "turn_id": msg.TurnID, "text": f.reply})
			write(map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": msg.TurnID, "snapshot": map[string]any{"messages": []any{}}})
		case omp.OpReceipt:
			write(map[string]any{"seat": msg.Seat, "type": "receipt", "rpc_id": msg.RPCID, "request_id": msg.RequestID, "receipt": receipts[msg.RequestID]})
		}
	}
}

// nextLoad blocks for the next load or adopt; `go test -timeout` is the clock.
func (f *ompFixture) nextLoad(t *testing.T) omp.Message {
	t.Helper()
	return <-f.loads
}

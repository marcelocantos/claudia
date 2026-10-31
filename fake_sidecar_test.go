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

// fakeSidecar is one sidecar connection's worth of a fake: it loads a seat,
// answers each prompt with reply, and answers a switch (a load naming a
// request id) as the real sidecar does since 🎯T195.
type fakeSidecar struct {
	mu    sync.Mutex
	loads []omp.Message
	in    []omp.Message
	// refuseSwitch, when set, is the text of the refusal a switch gets.
	refuseSwitch string
	// compactReason, when set, is the sidecar error reason for OpCompact.
	compactReason string
	// promptHold blocks OpPrompt until closed.
	promptHold chan struct{}
	promptInFlight atomic.Bool
	compactDuringPrompt atomic.Bool
	compactDelay time.Duration
	compactMu     sync.Mutex
	compactCancels map[string]context.CancelFunc
	compactAbortsMu sync.Mutex
	compactAborts   []string
	compactSilent   bool
}

func (f *fakeSidecar) loaded() []omp.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]omp.Message(nil), f.loads...)
}

func (f *fakeSidecar) incoming() []omp.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]omp.Message(nil), f.in...)
}

// startFakeSidecar serves one sidecar connection, points direct starts at
// it, and puts anthropic and openai-codex plan tokens in a fake Keychain.
func startFakeSidecar(t *testing.T, reply []string) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{}
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-fake-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var msg omp.Message
			_ = json.Unmarshal(line, &msg)
			f.mu.Lock()
			f.in = append(f.in, msg)
			f.mu.Unlock()
			switch msg.Op {
			case omp.OpToken:
				// Plan token updates are fire-and-forget on the wire.
			case omp.OpLoad:
				f.mu.Lock()
				f.loads = append(f.loads, msg)
				refuse := f.refuseSwitch
				f.mu.Unlock()
				switch {
				case msg.RequestID == "":
					_, _ = fmt.Fprintf(c, "{\"type\":\"ready\",\"provider\":%q,\"how\":\"launched\",\"restored\":false}\n", msg.Provider)
				case refuse != "":
					b, _ := json.Marshal(map[string]any{"seat": msg.Seat, "type": "error", "reason": "switch_refused", "request_id": msg.RequestID, "text": refuse})
					_, _ = c.Write(append(b, '\n'))
				default:
					b, _ := json.Marshal(map[string]any{"seat": msg.Seat, "type": "ready", "how": "switched", "provider": msg.Provider, "request_id": msg.RequestID})
					_, _ = c.Write(append(b, '\n'))
				}
			case omp.OpCompactAbort:
				f.compactAbortsMu.Lock()
				f.compactAborts = append(f.compactAborts, msg.RequestID)
				f.compactAbortsMu.Unlock()
				f.compactMu.Lock()
				if cancel := f.compactCancels[msg.RequestID]; cancel != nil {
					cancel()
				}
				f.compactMu.Unlock()
			case omp.OpCompact:
				go func(msg omp.Message) {
					if f.promptInFlight.Load() {
						f.compactDuringPrompt.Store(true)
						b, _ := json.Marshal(map[string]any{"seat": msg.Seat, "type": "error", "request_id": msg.RequestID, "reason": "turn_in_flight", "text": "compact refused: turn in flight"})
						_, _ = c.Write(append(b, '\n'))
						return
					}
					ctx, cancel := context.WithCancel(context.Background())
					f.compactMu.Lock()
					if f.compactCancels == nil {
						f.compactCancels = map[string]context.CancelFunc{}
					}
					f.compactCancels[msg.RequestID] = cancel
					f.compactMu.Unlock()
					defer func() {
						f.compactMu.Lock()
						delete(f.compactCancels, msg.RequestID)
						f.compactMu.Unlock()
					}()
					if f.compactSilent {
						<-ctx.Done()
						return
					}
					if f.compactDelay > 0 {
						select {
						case <-time.After(f.compactDelay):
						case <-ctx.Done():
							b, _ := json.Marshal(map[string]any{"seat": msg.Seat, "type": "error", "request_id": msg.RequestID, "reason": "compact_canceled", "text": "compact canceled"})
							_, _ = c.Write(append(b, '\n'))
							return
						}
					}
					if ctx.Err() != nil {
						b, _ := json.Marshal(map[string]any{"seat": msg.Seat, "type": "error", "request_id": msg.RequestID, "reason": "compact_canceled", "text": "compact canceled"})
						_, _ = c.Write(append(b, '\n'))
						return
					}
					if f.compactReason != "" {
						b, _ := json.Marshal(map[string]any{"seat": msg.Seat, "type": "error", "request_id": msg.RequestID, "reason": f.compactReason, "text": "compact refused"})
						_, _ = c.Write(append(b, '\n'))
						return
					}
					b, _ := json.Marshal(map[string]any{
						"seat": msg.Seat, "type": "compact_result", "request_id": msg.RequestID,
						"tokens_before": 200000, "tokens_after": 120000, "committed": true,
					})
					_, _ = c.Write(append(b, '\n'))
				}(msg)
			case omp.OpPrompt:
				go func(turnID string) {
					f.promptInFlight.Store(true)
					if f.promptHold != nil {
						<-f.promptHold
					}
					for _, l := range reply {
						_, _ = c.Write([]byte(withTurnID(l, turnID) + "\n"))
					}
					f.promptInFlight.Store(false)
				}(msg.TurnID)
			}
		}
	}()
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)
	stubOMPPlans(t)
	return f
}

// stubOMPPlans puts anthropic and openai-codex plan tokens in a fake
// Keychain and opens the plans, so a direct start reaches the sidecar.
func stubOMPPlans(t *testing.T) {
	t.Helper()
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{` +
		`"anthropic":{"refresh_token":"r","access_token":"plan-token","expiry":"` + exp + `"},` +
		`"openai-codex":{"refresh_token":"r2","access_token":"codex-token","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }
	t.Cleanup(func() { ompKeychain = nil; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// withTurnID stamps the prompt's turn identity onto a fixture event, as the
// real sidecar does since 🎯T179 (events without one are refused).
func withTurnID(ev, turnID string) string {
	var m map[string]any
	if json.Unmarshal([]byte(ev), &m) != nil {
		return ev
	}
	if _, ok := m["turn_id"]; ok {
		return ev
	}
	m["turn_id"] = turnID
	b, err := json.Marshal(m)
	if err != nil {
		return ev
	}
	return string(b)
}

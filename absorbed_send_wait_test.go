// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// startAbsorbingSidecar answers a prompt that arrives while turn A is open
// the way seat.ts does: the prompt is queued, the running turn absorbs it
// (an "absorbed" event stamped with A's turn id), answers it, and A ends.
// No turn ever carries the second prompt's own turn id.
func startAbsorbingSidecar(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "c3abs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
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
		var turnA string
		var writeMu sync.Mutex
		write := func(v any) {
			b, _ := json.Marshal(v)
			writeMu.Lock()
			_, _ = c.Write(append(b, '\n'))
			writeMu.Unlock()
		}
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var msg omp.Message
			if json.Unmarshal(line, &msg) != nil {
				continue
			}
			switch msg.Op {
			case omp.OpLoad:
				write(map[string]any{"type": "ready", "provider": msg.Provider, "how": "launched", "restored": false})
			case omp.OpPrompt:
				if turnA == "" {
					turnA = msg.TurnID
					write(map[string]any{"seat": msg.Seat, "type": "text", "text": "working on A", "turn_id": turnA})
					continue
				}
				write(map[string]any{"seat": msg.Seat, "type": "accepted"})
				write(map[string]any{"seat": msg.Seat, "type": "absorbed", "text": msg.Text, "turn_id": turnA})
				write(map[string]any{"seat": msg.Seat, "type": "text", "text": "answer B", "turn_id": turnA})
				write(map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnA})
			}
		}
	}()
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)
	stubOMPPlans(t)
}

// A Send to a busy sidecar seat is absorbed into the open turn, and that
// turn's end answers it. The C3 turn filter must not make the wait for the
// absorbed send ignore the only turn that will ever answer it.
func TestSendAbsorbedIntoOpenTurnIsAnsweredByThatTurn(t *testing.T) {
	startAbsorbingSidecar(t)
	a, err := StartDirect(Config{
		Name: "absorb", Provider: Provider(omp.Anthropic), Model: "claude-opus",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Stop() })
	if err := a.WaitReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	sawA := make(chan struct{}, 1)
	a.SubscribeEvents(func(ev Event) {
		if ev.Type == "assistant" && ev.Text == "working on A" {
			select {
			case sawA <- struct{}{}:
			default:
			}
		}
	})
	if err := a.Send("question A"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sawA:
	case <-t.Context().Done():
		t.Fatal("turn A never started")
	}
	if err := a.Send("question B"); err != nil {
		t.Fatal(err)
	}
	got, err := a.WaitForResponse(t.Context())
	if err != nil || !strings.Contains(got, "answer B") {
		t.Fatalf("WaitForResponse after a Send absorbed into the open turn = %q, %v; want the absorbing turn's answer", got, err)
	}
}

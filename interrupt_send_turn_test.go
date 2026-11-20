// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// startInterruptSidecar is the hunt's wire for C3. The first prompt is turn
// A and stays open. endOnSecondPrompt reports A's aborted turn_end when B's
// prompt is read, which is the order Interrupt-then-Send produces when the
// abort's echo is still in flight as Send returns. Otherwise the second
// prompt is only accepted, and OpAbort emits A's end, which is the
// escalation ladder: the waiter is already subscribed when the interrupt fires.
func startInterruptSidecar(t *testing.T, endOnSecondPrompt bool, answerAfter time.Duration) {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "c3-")
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
		var turnA, turnB string
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
				turnB = msg.TurnID
				if !endOnSecondPrompt {
					write(map[string]any{"seat": msg.Seat, "type": "accepted"})
					continue
				}
				finishB := func() {
					time.Sleep(answerAfter)
					write(map[string]any{"seat": msg.Seat, "type": "text", "text": "answer B", "turn_id": turnB})
					write(map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnB})
				}
				write(map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnA, "stop_reason": "aborted"})
				go finishB()
			case omp.OpAbort:
				if endOnSecondPrompt || turnA == "" {
					continue
				}
				write(map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnA, "stop_reason": "aborted"})
				go func() {
					time.Sleep(answerAfter)
					write(map[string]any{"seat": msg.Seat, "type": "text", "text": "answer B", "turn_id": turnB})
					write(map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnB})
				}()
			}
		}
	}()
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)
	stubOMPPlans(t)
}

func TestInterruptThenSendReturnsTheNewTurnsAnswer(t *testing.T) {
	startInterruptSidecar(t, true, 600*time.Millisecond)
	a, err := StartDirect(Config{
		Name: "hunt", Provider: Provider(omp.Anthropic), Model: "claude-opus",
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
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if err := a.Send("question B"); err != nil {
		t.Fatal(err)
	}
	got, err := a.WaitForResponse(t.Context())
	if err != nil || got != "answer B" {
		t.Fatalf("WaitForResponse after Interrupt+Send(B) = %q, %v; want %q", got, err, "answer B")
	}
}

func TestEscalatedMessageWaitGetsItsOwnAnswer(t *testing.T) {
	startInterruptSidecar(t, false, 600*time.Millisecond)
	a, err := StartDirect(Config{
		Name: "hunt-escalate", Provider: Provider(omp.Anthropic), Model: "claude-opus",
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
		if ev.Text == "working on A" {
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
	if a.TurnPhase() != TurnInTurn {
		t.Fatal("fixture: seat should be busy")
	}
	if _, err := a.SendEscalating("urgent B", Escalation{
		{Mode: DeliverySubmit},
		{Mode: DeliveryInterrupt, After: 200 * time.Millisecond},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := a.WaitForResponse(t.Context())
	if err != nil || got != "answer B" {
		t.Fatalf("wait for escalated message = %q, %v; want %q", got, err, "answer B")
	}
}

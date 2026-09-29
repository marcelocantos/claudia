// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// escalationSeat is a stub whose verbs and phase the test observes.
type escalationSeat struct {
	mu    sync.Mutex
	phase TurnPhase
	ran   []string
}

func (s *escalationSeat) record(v string) {
	s.mu.Lock()
	s.ran = append(s.ran, v)
	s.mu.Unlock()
}

func (s *escalationSeat) verbs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ran...)
}

func newEscalationSeat(phase TurnPhase) (*Agent, *escalationSeat) {
	s := &escalationSeat{phase: phase}
	a := NewStubAgentOps(&StubAgentOps{
		Provider: ProviderCursor,
		Send:     func(text string) error { s.record("send:" + text); return nil },
		Steer: func(text string) (DeliveryOutcome, error) {
			s.record("steer:" + text)
			return DeliveryOutcome{Mechanism: "stub_steer"}, nil
		},
		Interrupt: func() error { s.record("interrupt"); return nil },
		TurnPhase: func() TurnPhase {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.phase
		},
	})
	return a, s
}

// 🎯T138: the ladder's shape is checked before anything is sent.
func TestEscalationValidate(t *testing.T) {
	ok := Escalation{{Mode: DeliverySteer}, {Mode: DeliveryInterrupt, After: time.Minute}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("steer then interrupt: %v", err)
	}
	for name, bad := range map[string]Escalation{
		"interrupt first":       {{Mode: DeliveryInterrupt}},
		"steer after the first": {{Mode: DeliverySubmit}, {Mode: DeliverySteer, After: time.Second}},
		"deadlines go backward": {{Mode: DeliverySteer, After: time.Minute}, {Mode: DeliveryInterrupt, After: time.Second}},
		"too many rungs":        {{Mode: DeliverySteer}, {Mode: DeliveryInterrupt}, {Mode: DeliveryInterrupt}, {Mode: DeliveryInterrupt}, {Mode: DeliveryInterrupt}},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrBadEscalation) {
			t.Errorf("%s: err = %v, want ErrBadEscalation", name, err)
		}
	}
}

func TestEscalationIdleSeatIsAPlainSubmit(t *testing.T) {
	a, s := newEscalationSeat(TurnIdle)
	out, err := a.SendEscalating("hi", Escalation{{Mode: DeliverySteer}, {Mode: DeliveryInterrupt, After: time.Millisecond}})
	if err != nil || out.Mechanism != MechanismSubmit {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := s.verbs(); len(got) != 1 || got[0] != "send:hi" {
		t.Fatalf("verbs = %v, want [send:hi]", got)
	}
}

// Absorbed before the deadline: the soft rung was enough, nothing interrupts.
func TestEscalationAbsorbedMessageIsNotInterrupted(t *testing.T) {
	a, s := newEscalationSeat(TurnInTurn)
	if _, err := a.SendEscalating("status?", Escalation{{Mode: DeliverySteer}, {Mode: DeliveryInterrupt, After: 80 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	a.publishEvent(Event{Type: "progress", ProgressType: ProgressDeliveryAbsorbed, Text: "some other message"})
	a.publishEvent(Event{Type: "progress", ProgressType: ProgressDeliveryAbsorbed, Text: "status?"})
	time.Sleep(160 * time.Millisecond)
	if got := s.verbs(); len(got) != 1 || got[0] != "steer:status?" {
		t.Fatalf("verbs = %v, want [steer:status?]", got)
	}
}

// The turn it waited behind ended: the seat takes it next, nothing interrupts.
func TestEscalationTurnEndSettlesTheLadder(t *testing.T) {
	a, s := newEscalationSeat(TurnInTurn)
	if _, err := a.SendEscalating("later", Escalation{{Mode: DeliverySubmit}, {Mode: DeliveryInterrupt, After: 80 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	a.publishEvent(Event{Type: "assistant", StopReason: "end_turn"})
	time.Sleep(160 * time.Millisecond)
	for _, v := range s.verbs() {
		if v == "interrupt" {
			t.Fatalf("interrupted after the turn ended: %v", s.verbs())
		}
	}
}

// Not absorbed by the deadline: the interrupt rung fires, once, and says so.
func TestEscalationUnabsorbedMessageIsInterruptedAtItsDeadline(t *testing.T) {
	a, s := newEscalationSeat(TurnInTurn)
	escalated := make(chan string, 4)
	a.SubscribeEvents(func(ev Event) {
		if ev.Type == "progress" && ev.ProgressType == ProgressDeliveryEscalated {
			escalated <- ev.Text
		}
	})
	start := time.Now()
	if _, err := a.SendEscalating("urgent", Escalation{{Mode: DeliverySteer}, {Mode: DeliveryInterrupt, After: 60 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	// Blocks until the rung fires; `go test -timeout` is the clock.
	mode := <-escalated
	if mode != string(DeliveryInterrupt) {
		t.Fatalf("escalated with %q", mode)
	}
	// 🎯T97 exemption: a lower bound. A slow host only lengthens `waited`, so
	// it cannot fail a ladder that waits for its deadline.
	if waited := time.Since(start); waited < 60*time.Millisecond {
		t.Fatalf("interrupted after %s, before its deadline", waited)
	}
	time.Sleep(30 * time.Millisecond)
	got := s.verbs()
	if len(got) != 2 || got[0] != "steer:urgent" || got[1] != "interrupt" {
		t.Fatalf("verbs = %v, want [steer:urgent interrupt]", got)
	}
}

// A newer escalating send supersedes the pending ladder: the seat is pressed
// by the latest message only, never interrupted by a stale timer.
func TestEscalationNewerSendSupersedesPendingLadder(t *testing.T) {
	a, s := newEscalationSeat(TurnInTurn)
	if _, err := a.SendEscalating("first", Escalation{{Mode: DeliverySteer}, {Mode: DeliveryInterrupt, After: 60 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendEscalating("second", Escalation{{Mode: DeliverySteer}, {Mode: DeliveryInterrupt, After: 400 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	for _, v := range s.verbs() {
		if v == "interrupt" {
			t.Fatalf("the superseded ladder still interrupted: %v", s.verbs())
		}
	}
	time.Sleep(400 * time.Millisecond)
	got := s.verbs()
	if len(got) != 3 || got[0] != "steer:first" || got[1] != "steer:second" || got[2] != "interrupt" {
		t.Fatalf("verbs = %v, want the second ladder's interrupt only", got)
	}
}

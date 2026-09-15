// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// verbLog records which delivery verbs a stub saw, in order.
type verbLog struct {
	mu    sync.Mutex
	verbs []string
	phase TurnPhase
}

func (l *verbLog) add(v string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verbs = append(l.verbs, v)
}

func (l *verbLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.verbs...)
}

func (l *verbLog) setPhase(p TurnPhase) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.phase = p
}

func (l *verbLog) getPhase() TurnPhase {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.phase == "" {
		return TurnIdle
	}
	return l.phase
}

func stubWithSteer(log *verbLog) *Agent {
	return NewStubAgentOps(&StubAgentOps{
		Provider: ProviderCursor,
		Send: func(text string) error {
			log.add("send:" + text)
			log.setPhase(TurnInTurn)
			return nil
		},
		Steer: func(text string) (DeliveryOutcome, error) {
			log.add("steer:" + text)
			return DeliveryOutcome{Mechanism: "fake_supersede", SupersededTurnID: "turn-1"}, nil
		},
		Interrupt: func() error {
			log.add("interrupt")
			log.setPhase(TurnIdle)
			return nil
		},
		TurnPhase: log.getPhase,
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SendMode(submit) is Send: same verb, same error propagation.
func TestSendModeSubmitEqualsSend(t *testing.T) {
	t.Parallel()
	log := &verbLog{}
	a := stubWithSteer(log)
	out, err := a.SendMode("hello", DeliverySubmit)
	if err != nil {
		t.Fatalf("SendMode(submit): %v", err)
	}
	if out.Mode != DeliverySubmit || out.Mechanism != MechanismSubmit || out.PhaseBefore != TurnIdle {
		t.Errorf("outcome = %+v, want submit/submit/idle", out)
	}
	if err := a.Send("again"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, want := log.got(), []string{"send:hello", "send:again"}; !equalStrings(got, want) {
		t.Errorf("verbs = %v, want %v", got, want)
	}

	wantErr := errors.New("turn not submitted")
	failing := NewStubAgentOps(&StubAgentOps{Send: func(string) error { return wantErr }})
	out, err = failing.SendMode("x", DeliverySubmit)
	if !errors.Is(err, wantErr) || !errors.Is(out.Err, wantErr) {
		t.Errorf("SendMode(submit) err = %v / outcome.Err = %v, want %v", err, out.Err, wantErr)
	}
	if sendErr := failing.Send("x"); !errors.Is(sendErr, wantErr) {
		t.Errorf("Send err = %v, want %v", sendErr, wantErr)
	}
	// An empty mode is the default, not an unknown mode.
	if _, err := a.SendMode("y", ""); err != nil {
		t.Errorf("SendMode(\"\") = %v, want submit", err)
	}
}

// SendMode(interrupt) hard-stops the open turn, then submits — and on an
// idle seat it is a plain submit with no interrupt at all.
func TestSendModeInterruptThenSubmit(t *testing.T) {
	t.Parallel()
	log := &verbLog{}
	a := stubWithSteer(log)
	log.setPhase(TurnInTurn)
	out, err := a.SendMode("stop and do this", DeliveryInterrupt)
	if err != nil {
		t.Fatalf("SendMode(interrupt): %v", err)
	}
	if out.Mechanism != MechanismInterruptThenSubmit || out.PhaseBefore != TurnInTurn {
		t.Errorf("outcome = %+v, want interrupt+submit from in_turn", out)
	}
	if got, want := log.got(), []string{"interrupt", "send:stop and do this"}; !equalStrings(got, want) {
		t.Errorf("verbs = %v, want %v", got, want)
	}

	log2 := &verbLog{}
	idle := stubWithSteer(log2)
	out, err = idle.SendMode("fresh", DeliveryInterrupt)
	if err != nil {
		t.Fatalf("SendMode(interrupt) idle: %v", err)
	}
	if out.Mechanism != MechanismSubmit || out.PhaseBefore != TurnIdle {
		t.Errorf("idle outcome = %+v, want plain submit", out)
	}
	if got, want := log2.got(), []string{"send:fresh"}; !equalStrings(got, want) {
		t.Errorf("idle verbs = %v, want %v", got, want)
	}

	// Empty text with interrupt: hard-stop only, nothing submitted.
	log3 := &verbLog{}
	busy := stubWithSteer(log3)
	log3.setPhase(TurnInTurn)
	if _, err := busy.SendMode("", DeliveryInterrupt); err != nil {
		t.Fatalf("SendMode(interrupt, empty): %v", err)
	}
	if got, want := log3.got(), []string{"interrupt"}; !equalStrings(got, want) {
		t.Errorf("empty-text verbs = %v, want %v", got, want)
	}

	// The provider closes the turn a beat after the interrupt RPC returns
	// (Codex turn/completed, ACP cancelled prompt): the submit waits for
	// the seat to read idle instead of landing on the still-open turn.
	log5 := &verbLog{}
	log5.setPhase(TurnInTurn)
	lagging := NewStubAgentOps(&StubAgentOps{
		Send: func(text string) error {
			log5.add("send:" + text + "@" + string(log5.getPhase()))
			return nil
		},
		Interrupt: func() error {
			log5.add("interrupt")
			go func() {
				time.Sleep(50 * time.Millisecond)
				log5.setPhase(TurnIdle)
			}()
			return nil
		},
		TurnPhase: log5.getPhase,
	})
	if _, err := lagging.SendMode("after settle", DeliveryInterrupt); err != nil {
		t.Fatalf("SendMode(interrupt) lagging: %v", err)
	}
	if got, want := log5.got(), []string{"interrupt", "send:after settle@idle"}; !equalStrings(got, want) {
		t.Errorf("lagging verbs = %v, want %v", got, want)
	}

	// An interrupt failure aborts before any submit.
	log4 := &verbLog{}
	broken := NewStubAgentOps(&StubAgentOps{
		Send:      func(text string) error { log4.add("send:" + text); return nil },
		Interrupt: func() error { return errors.New("escape lost") },
		TurnPhase: func() TurnPhase { return TurnInTurn },
	})
	if _, err := broken.SendMode("never", DeliveryInterrupt); err == nil {
		t.Fatal("SendMode(interrupt) returned nil while Interrupt failed")
	}
	if got := log4.got(); len(got) != 0 {
		t.Errorf("submit ran after a failed interrupt: %v", got)
	}
}

// Steer on a handle with no steer mechanism is a typed refusal with the
// mechanism recorded, on every provider whose contract cannot steer and
// on one whose seam is not wired.
func TestSteerUnsupportedIsTyped(t *testing.T) {
	t.Parallel()
	for _, provider := range []Provider{ProviderClaude, ProviderCursor, ProviderGrok, ProviderCodex, ProviderBedrock} {
		a := NewStubAgentOps(&StubAgentOps{
			Provider:  provider,
			TurnPhase: func() TurnPhase { return TurnInTurn },
		})
		out, err := a.Steer("nudge")
		if !errors.Is(err, ErrSteerUnsupported) {
			t.Errorf("%s: Steer err = %v, want ErrSteerUnsupported", provider, err)
		}
		if out.Mechanism != MechanismSteerUnsupported || out.Mode != DeliverySteer || !errors.Is(out.Err, ErrSteerUnsupported) {
			t.Errorf("%s: outcome = %+v", provider, out)
		}
		if caps := a.TurnCaps(); caps.CanSteer {
			t.Errorf("%s: TurnCaps.CanSteer true on a handle whose Steer refuses", provider)
		}
		// SendMode(steer) while in_turn reports the same refusal.
		if _, err := a.SendMode("nudge", DeliverySteer); !errors.Is(err, ErrSteerUnsupported) {
			t.Errorf("%s: SendMode(steer) err = %v", provider, err)
		}
	}
}

// Steer reaches the wired mechanism when a turn is open, refuses with
// ErrTurnIdle when it is not, and SendMode(steer) turns that idle case
// into a submit.
func TestSteerWiredFoldsIntoOpenTurn(t *testing.T) {
	t.Parallel()
	log := &verbLog{}
	a := stubWithSteer(log)

	if _, err := a.Steer("too early"); !errors.Is(err, ErrTurnIdle) {
		t.Fatalf("Steer idle err = %v, want ErrTurnIdle", err)
	}
	out, err := a.SendMode("first", DeliverySteer)
	if err != nil || out.Mechanism != MechanismSubmit {
		t.Fatalf("SendMode(steer) idle = %+v / %v, want plain submit", out, err)
	}

	out, err = a.Steer("actually, use Go")
	if err != nil {
		t.Fatalf("Steer in_turn: %v", err)
	}
	if out.Mechanism != "fake_supersede" || out.SupersededTurnID != "turn-1" || out.PhaseBefore != TurnInTurn {
		t.Errorf("Steer outcome = %+v", out)
	}
	out, err = a.SendMode("and tests", DeliverySteer)
	if err != nil || out.Mechanism != "fake_supersede" {
		t.Errorf("SendMode(steer) in_turn = %+v / %v", out, err)
	}
	if got, want := log.got(), []string{"send:first", "steer:actually, use Go", "steer:and tests"}; !equalStrings(got, want) {
		t.Errorf("verbs = %v, want %v", got, want)
	}
	if caps := a.TurnCaps(); !caps.CanSteer || !caps.CanInterrupt || caps.SteerPolicy != SteerBreakpoint {
		t.Errorf("TurnCaps = %+v, want Cursor contract with steer wired", caps)
	}
}

// Queue never touches the wire and says so.
func TestSendModeQueueIsHostSide(t *testing.T) {
	t.Parallel()
	log := &verbLog{}
	a := stubWithSteer(log)
	out, err := a.SendMode("hold this", DeliveryQueue)
	if !errors.Is(err, ErrQueueHostSide) {
		t.Fatalf("SendMode(queue) err = %v, want ErrQueueHostSide", err)
	}
	if out.Mechanism != MechanismClientQueue {
		t.Errorf("outcome = %+v", out)
	}
	if got := log.got(); len(got) != 0 {
		t.Errorf("queue reached the wire: %v", got)
	}
	if _, err := a.SendMode("x", DeliveryMode("teleport")); !errors.Is(err, ErrUnknownDeliveryMode) {
		t.Errorf("unknown mode err = %v", err)
	}
}

// A nil handle and a bare stub answer TurnPhase / TurnCaps without
// panicking, and a stub with no interrupt hook does not claim one.
func TestTurnPhaseAndCapsDefaults(t *testing.T) {
	t.Parallel()
	var nilAgent *Agent
	if nilAgent.TurnPhase() != TurnIdle {
		t.Error("nil TurnPhase != idle")
	}
	if caps := nilAgent.TurnCaps(); caps.CanInterrupt || caps.CanSteer || caps.SteerPolicy != SteerNone {
		t.Errorf("nil TurnCaps = %+v", caps)
	}
	bare := NewStubAgent(nil)
	if bare.TurnPhase() != TurnIdle {
		t.Error("bare stub TurnPhase != idle")
	}
	caps := bare.TurnCaps()
	if caps.CanInterrupt || caps.CanSteer || caps.SteerPolicy != SteerQueueUntilIdle || caps.BusyOnSecondSubmit != BusySubmitQueue {
		t.Errorf("bare stub TurnCaps = %+v, want Claude contract minus unwired verbs", caps)
	}
	if NewStubAgentOps(nil).TurnPhase() != TurnIdle {
		t.Error("NewStubAgentOps(nil) TurnPhase != idle")
	}
}

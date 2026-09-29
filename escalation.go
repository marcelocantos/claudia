// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// An escalation protocol lets a host say how hard to press a busy seat with
// one message (🎯T138): take it softly first, and if the seat has not taken
// it by a deadline, interrupt the turn so it does. The host chooses the
// ladder, which is its judgement of urgency. Claudia runs it, because only
// Claudia sees the provider's turn and whether the message was absorbed.

// Progress types published while an escalation runs.
const (
	// ProgressDeliveryAbsorbed: the seat's model took a message that was
	// queued or steered behind a running turn. Text is the message.
	ProgressDeliveryAbsorbed = "delivery_absorbed"
	// ProgressDeliveryEscalated: an escalation rung fired on a message the
	// seat had not absorbed. Text names the rung's mode.
	ProgressDeliveryEscalated = "delivery_escalated"
)

// EscalationStep is one rung: deliver with Mode once After has passed since
// the send without the message being absorbed.
type EscalationStep struct {
	Mode  DeliveryMode
	After time.Duration
}

// Escalation is an ordered ladder. The first rung runs at once and must be
// [DeliverySubmit] or [DeliverySteer]. Later rungs escalate an unabsorbed
// message and must be [DeliveryInterrupt]: the text is already with the
// seat, so a later submit or steer would only deliver it twice. After must
// not decrease from rung to rung.
type Escalation []EscalationStep

// maxEscalationSteps bounds a ladder; a longer one is a caller bug.
const maxEscalationSteps = 4

// ErrBadEscalation: the ladder breaks one of [Escalation]'s rules.
var ErrBadEscalation = errors.New("bad escalation")

// Validate checks the ladder's shape.
func (e Escalation) Validate() error {
	if len(e) > maxEscalationSteps {
		return fmt.Errorf("%w: %d rungs, at most %d", ErrBadEscalation, len(e), maxEscalationSteps)
	}
	var prev time.Duration
	for i, s := range e {
		if s.After < prev {
			return fmt.Errorf("%w: rung %d fires at %s, before rung %d at %s", ErrBadEscalation, i, s.After, i-1, prev)
		}
		prev = s.After
		if i == 0 {
			if s.Mode != DeliverySubmit && s.Mode != DeliverySteer {
				return fmt.Errorf("%w: the first rung is submit or steer, not %q", ErrBadEscalation, s.Mode)
			}
			continue
		}
		if s.Mode != DeliveryInterrupt {
			return fmt.Errorf("%w: rung %d is %q; only interrupt escalates a message already with the seat", ErrBadEscalation, i, s.Mode)
		}
	}
	return nil
}

// SendEscalating delivers text under an escalation protocol. With no
// ladder, or on an idle seat, it is [Agent.Send]. On a busy seat the first
// rung delivers the text now; each later rung interrupts the turn at its
// deadline unless the message was absorbed first — the seat's model took
// it (a [ProgressDeliveryAbsorbed] event carrying this text) or the turn
// ended. The returned outcome is the first rung's; rungs that fire later
// are published as [ProgressDeliveryEscalated] events.
func (a *Agent) SendEscalating(text string, esc Escalation) (DeliveryOutcome, error) {
	if err := esc.Validate(); err != nil {
		out := DeliveryOutcome{Mode: DeliverySubmit, Mechanism: MechanismNone, Err: err}
		return out, err
	}
	if a.ops.sendEscalating != nil {
		// A broker handle: the daemon holding the seat runs the ladder, so
		// it outlives this host's connection.
		return a.ops.sendEscalating(a, text, esc)
	}
	if len(esc) == 0 || a.TurnPhase() != TurnInTurn {
		return a.SendMode(text, DeliverySubmit)
	}
	if len(esc) == 1 {
		return a.SendMode(text, esc[0].Mode)
	}
	// Subscribe before delivering, so an absorb that follows at once is not
	// missed.
	absorbed := make(chan struct{})
	var once sync.Once
	token := a.SubscribeEvents(func(ev Event) {
		if absorbs(ev, text) {
			once.Do(func() { close(absorbed) })
		}
	})
	start := time.Now()
	out, err := a.SendMode(text, esc[0].Mode)
	if err != nil {
		a.UnsubscribeEvents(token)
		return out, err
	}
	go func() {
		defer a.UnsubscribeEvents(token)
		a.climb(esc[1:], start, absorbed)
	}()
	return out, nil
}

// climb fires each remaining rung at its deadline unless the message was
// absorbed first, or the seat stopped.
func (a *Agent) climb(rungs Escalation, start time.Time, absorbed <-chan struct{}) {
	for _, r := range rungs {
		timer := time.NewTimer(time.Until(start.Add(r.After)))
		select {
		case <-absorbed:
			timer.Stop()
			return
		case <-a.deadSignal():
			timer.Stop()
			return
		case <-timer.C:
		}
		if a.TurnPhase() != TurnInTurn {
			return // the turn ended; whatever was queued is taken next
		}
		a.publishEvent(Event{Type: "progress", ProgressType: ProgressDeliveryEscalated, Text: string(r.Mode)})
		// Interrupt only: the text is already with the seat. A provider that
		// keeps queued messages across a hard-stop takes it at once (the Oh
		// My Pi sidecar drains its queue as a new turn); one that folded the
		// steer into the turn already saw it.
		_ = a.Interrupt()
	}
}

// absorbs reports whether ev settles an escalation for text: the model took
// that message, or the turn it was waiting behind ended.
func absorbs(ev Event, text string) bool {
	if ev.Type == "progress" && ev.ProgressType == ProgressDeliveryAbsorbed {
		return ev.Text == text
	}
	return ev.IsTerminalStop()
}

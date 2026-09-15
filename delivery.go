// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeliveryMode selects the host intent for one user-text delivery
// (docs/design/steer-interrupt-turn-api.md). [Agent.Send] is
// [DeliverySubmit]; the other modes are reachable through
// [Agent.SendMode] and [Agent.Steer].
type DeliveryMode string

const (
	// DeliverySubmit starts a turn when idle. While a turn is open the
	// provider's own busy behaviour applies ([TurnCaps].BusyOnSecondSubmit);
	// claudia neither queues nor auto-steers on the caller's behalf.
	DeliverySubmit DeliveryMode = "submit"
	// DeliverySteer folds text into the open turn ([Agent.Steer]); when
	// the seat is idle it is a plain submit.
	DeliverySteer DeliveryMode = "steer"
	// DeliveryInterrupt hard-stops the open turn ([Agent.Interrupt]) and
	// then submits the text; when the seat is idle it is a plain submit.
	DeliveryInterrupt DeliveryMode = "interrupt"
	// DeliveryQueue is an explicit host-side enqueue. Nothing reaches the
	// wire: claudia holds no queue, so the call acknowledges with
	// [ErrQueueHostSide] and the caller keeps the text.
	DeliveryQueue DeliveryMode = "queue"
)

// Mechanism labels for [DeliveryOutcome].Mechanism: what actually ran.
// The ACP supersede label is minted where that mechanism lives
// (acp_steer.go, 🎯T72.1) and flows through the seam unchanged.
const (
	// MechanismSubmit: the text went through the provider's ordinary
	// submit path.
	MechanismSubmit = "submit"
	// MechanismInterruptThenSubmit: the provider hard-stop ran, then the
	// ordinary submit path.
	MechanismInterruptThenSubmit = "interrupt+submit"
	// MechanismClientQueue: nothing reached the wire; the caller holds
	// the text.
	MechanismClientQueue = "client_queue"
	// MechanismCodexTurnSteer: Codex app-server turn/steer against the
	// open turn.
	MechanismCodexTurnSteer = "codex_turn_steer"
	// MechanismSteerUnsupported: Steer was asked of a handle with no steer
	// mechanism; nothing reached the wire.
	MechanismSteerUnsupported = "steer_unsupported"
	// MechanismNone: nothing ran (the request was refused before any
	// mechanism was chosen).
	MechanismNone = ""
)

// DeliveryOutcome is returned synchronously from [Agent.SendMode] and
// [Agent.Steer]. Mechanism records what ran, for logs and UI hints; it is
// not a second source of truth about the turn — events are.
type DeliveryOutcome struct {
	Mode        DeliveryMode
	PhaseBefore TurnPhase
	// Mechanism names what actually ran: one of the Mechanism constants,
	// or a provider-minted label such as the ACP supersede.
	Mechanism string
	// SupersededTurnID is set when a steer superseded an in-flight prompt
	// RPC (ACP); empty for mechanisms that fold text into the same turn.
	SupersededTurnID string
	// Err mirrors the returned error so an outcome can be logged whole.
	Err error
}

var (
	// ErrSteerUnsupported: this handle has no steer mechanism. Either the
	// provider cannot steer at all ([TurnCaps].CanSteer false on its
	// contract) or the mechanism is not wired for this handle. The caller
	// queues, or interrupts and resubmits.
	ErrSteerUnsupported = errors.New("steer unsupported")
	// ErrTurnIdle: [Agent.Steer] was called with no turn open. Steer folds
	// text into a running turn; an idle seat wants [Agent.Send].
	ErrTurnIdle = errors.New("no turn in flight to steer")
	// ErrQueueHostSide: [DeliveryQueue] was requested. claudia holds no
	// queue; the text was not sent and the caller keeps it.
	ErrQueueHostSide = errors.New("queue is host-side; text not sent")
	// ErrUnknownDeliveryMode: a [DeliveryMode] outside the enum.
	ErrUnknownDeliveryMode = errors.New("unknown delivery mode")
)

// TurnPhase reports whether the seat has a turn open right now. The
// broker handle asks the daemon; direct handles read the provider's
// prompt-in-flight signal ([Agent.PromptInFlight]). Unknown reads as
// [TurnIdle], which is why [DeliveryInterrupt] on an unknowable phase is
// a plain submit rather than a spurious hard-stop.
func (a *Agent) TurnPhase() TurnPhase {
	if a == nil {
		return TurnIdle
	}
	if a.ops.turnPhase != nil {
		if phase := a.ops.turnPhase(a); phase == TurnInTurn {
			return TurnInTurn
		}
		return TurnIdle
	}
	if a.PromptInFlight() {
		return TurnInTurn
	}
	return TurnIdle
}

// TurnCaps reports what this handle can do to an open turn. It starts
// from the provider contract ([ProviderTurnCaps]), lets the backend
// refine it (a Codex CLI whose schema lacks turn/steer), and withdraws
// the steer claim whenever no steer mechanism is wired — so CanSteer true
// always means [Agent.Steer] reaches the wire.
func (a *Agent) TurnCaps() TurnCaps {
	if a == nil {
		return TurnCaps{SteerPolicy: SteerNone}
	}
	caps := ProviderTurnCaps(a.Provider())
	if a.ops.turnCaps != nil {
		caps = a.ops.turnCaps(a)
	}
	if a.ops.steer == nil {
		caps = caps.withoutSteer()
	}
	if a.ops.interrupt == nil {
		caps.CanInterrupt = false
	}
	return caps
}

// SendMode delivers text with an explicit [DeliveryMode].
// SendMode(text, DeliverySubmit) is [Agent.Send]. [DeliverySteer] and
// [DeliveryInterrupt] on an idle seat are a plain submit; on an open turn
// they run [Agent.Steer] and [Agent.Interrupt]-then-submit respectively.
// [DeliveryQueue] sends nothing and returns [ErrQueueHostSide].
//
// The returned outcome is also carried in its Err field, so a caller can
// log the whole outcome and still use the error idiomatically.
func (a *Agent) SendMode(text string, mode DeliveryMode) (DeliveryOutcome, error) {
	if mode == "" {
		mode = DeliverySubmit
	}
	out := DeliveryOutcome{Mode: mode, PhaseBefore: a.TurnPhase()}
	switch mode {
	case DeliverySubmit:
		out.Mechanism = MechanismSubmit
		out.Err = a.Send(text)
	case DeliverySteer:
		if out.PhaseBefore != TurnInTurn {
			out.Mechanism = MechanismSubmit
			out.Err = a.Send(text)
			break
		}
		steered, err := a.steer(text, out.PhaseBefore)
		out.Mechanism, out.SupersededTurnID, out.Err = steered.Mechanism, steered.SupersededTurnID, err
	case DeliveryInterrupt:
		out.Mechanism = MechanismSubmit
		if out.PhaseBefore == TurnInTurn {
			out.Mechanism = MechanismInterruptThenSubmit
			if err := a.Interrupt(); err != nil {
				out.Err = fmt.Errorf("interrupt before submit: %w", err)
				return out, out.Err
			}
			a.awaitIdle(interruptSettleTimeout)
		}
		if text != "" {
			out.Err = a.Send(text)
		}
	case DeliveryQueue:
		out.Mechanism = MechanismClientQueue
		out.Err = ErrQueueHostSide
	default:
		out.Mechanism = MechanismNone
		out.Err = fmt.Errorf("%w: %q", ErrUnknownDeliveryMode, mode)
	}
	return out, out.Err
}

// interruptSettleTimeout bounds how long SendMode(DeliveryInterrupt)
// waits for the seat to read idle after the hard-stop before submitting.
// The interrupt RPC settles before the turn's terminal notification does
// (Codex answers turn/interrupt, then emits turn/completed; ACP answers
// session/cancel, then the prompt RPC returns cancelled), so a submit on
// the RPC's heels lands on a turn the provider still counts as open.
const interruptSettleTimeout = 5 * time.Second

// interruptSettlePoll is the phase re-read interval inside awaitIdle.
const interruptSettlePoll = 20 * time.Millisecond

// awaitIdle returns once TurnPhase reads idle or timeout elapses. It
// never fails: a seat that stays busy is left to Send, whose provider
// error names the busy turn honestly.
func (a *Agent) awaitIdle(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for a.TurnPhase() == TurnInTurn && time.Now().Before(deadline) {
		time.Sleep(interruptSettlePoll)
	}
}

// Steer folds text into the running turn without tearing it down. It
// returns [ErrSteerUnsupported] when this handle has no steer mechanism
// (see [Agent.TurnCaps]) and [ErrTurnIdle] when no turn is open — an idle
// seat wants [Agent.Send]. [Agent.SendMode] with [DeliverySteer] makes
// that idle case a submit automatically.
func (a *Agent) Steer(text string) (DeliveryOutcome, error) {
	return a.steer(text, a.TurnPhase())
}

func (a *Agent) steer(text string, phase TurnPhase) (DeliveryOutcome, error) {
	out := DeliveryOutcome{Mode: DeliverySteer, PhaseBefore: phase}
	if a.ops.steer == nil {
		out.Mechanism = MechanismSteerUnsupported
		reason := "steer mechanism not wired for this handle"
		if !ProviderTurnCaps(a.Provider()).CanSteer {
			reason = "provider has no steer mechanism (policy " + string(ProviderTurnCaps(a.Provider()).SteerPolicy) + ")"
		}
		out.Err = fmt.Errorf("%w: %s provider: %s", ErrSteerUnsupported, a.Provider(), reason)
		return out, out.Err
	}
	if err := a.deliverable(); err != nil {
		out.Mechanism = MechanismNone
		out.Err = err
		return out, out.Err
	}
	if phase != TurnInTurn {
		out.Mechanism = MechanismNone
		out.Err = ErrTurnIdle
		return out, out.Err
	}
	steered, err := a.ops.steer(a, text)
	out.Mechanism, out.SupersededTurnID = steered.Mechanism, steered.SupersededTurnID
	if err != nil {
		out.Err = err
		return out, out.Err
	}
	a.recordInert(inertTurn{Role: "user", Text: strings.TrimSpace(text)})
	return out, nil
}

// deliverable is the precondition Send and Steer share: the TUI has
// finished initialising and the process is still there. The error text
// is Send's, unchanged, so consumers matching on it keep working.
func (a *Agent) deliverable() error {
	<-a.ready
	if a.readyErr != nil {
		return fmt.Errorf("claude not ready: %w", a.readyErr)
	}
	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("claude process not running")
	}
	return nil
}

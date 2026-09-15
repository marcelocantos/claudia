// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

// NewStubAgent returns an Agent that reports itself alive and delivers Send
// to fn, without a process, a TUI, or a tmux window behind it.
//
// It exists because Alive() reads unexported state, so a dependent package
// had no way to build a server whose overseer is up. jevons gates owner
// sends on `proc == nil || !proc.Alive()` (its 🎯T545 rule: a down overseer
// is a nack, not a silent enqueue), and its tests faked the overseer with a
// zero &claudia.Agent{} — which reports NOT alive. Four of them went red on
// master the day that gate landed and stayed there, because the only way to
// satisfy the gate was to start a real agent.
//
// A stub is the honest seam for that: the test drives the same Send path the
// product does, rather than a parallel one that can drift away from it.
//
// fn may be nil, in which case Send succeeds and discards the message.
func NewStubAgent(fn func(string) error) *Agent {
	ready := make(chan struct{})
	close(ready) // Send waits on this; a stub is ready immediately.
	a := &Agent{alive: true, ready: ready}
	a.ops = agentOps{send: func(_ *Agent, msg string) error {
		if fn == nil {
			return nil
		}
		return fn(msg)
	}}
	return a
}

// StubAgentOps names the verbs a stub answers. Every field is optional:
// a nil Send succeeds and discards; a nil Steer makes [Agent.Steer]
// report [ErrSteerUnsupported]; a nil Interrupt makes [Agent.Interrupt]
// refuse; nil TurnPhase reads [TurnIdle]; nil TurnCaps reads the
// provider contract ([ProviderTurnCaps]) minus whatever is unwired.
//
// It is an args struct rather than functional options by house rule
// (go.md); every hook is visible in one place and a zero field is the
// documented default.
type StubAgentOps struct {
	// Provider the stub reports; empty means Claude.
	Provider  Provider
	Send      func(text string) error
	Steer     func(text string) (DeliveryOutcome, error)
	Interrupt func() error
	TurnPhase func() TurnPhase
	TurnCaps  func() TurnCaps
}

// NewStubAgentOps is [NewStubAgent] with every delivery verb observable
// (🎯T72.2): a dependent package can assert which of Send / Steer /
// Interrupt a broker or host path actually ran, on the same
// [Agent.SendMode] path the product uses. args may be nil.
func NewStubAgentOps(args *StubAgentOps) *Agent {
	if args == nil {
		args = &StubAgentOps{}
	}
	a := NewStubAgent(args.Send)
	a.provider = args.Provider
	if args.Steer != nil {
		a.ops.steer = func(_ *Agent, text string) (DeliveryOutcome, error) { return args.Steer(text) }
	}
	if args.Interrupt != nil {
		a.ops.interrupt = func(*Agent) error { return args.Interrupt() }
	}
	if args.TurnPhase != nil {
		a.ops.turnPhase = func(*Agent) TurnPhase { return args.TurnPhase() }
	}
	if args.TurnCaps != nil {
		a.ops.turnCaps = func(*Agent) TurnCaps { return args.TurnCaps() }
	}
	return a
}

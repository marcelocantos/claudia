// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"sync"
)

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
	// Provider the stub reports; empty means Claude. [StartStub] reports
	// Config.Provider instead.
	Provider  Provider
	Send      func(text string) error
	Steer     func(text string) (DeliveryOutcome, error)
	Interrupt func() error
	TurnPhase func() TurnPhase
	TurnCaps  func() TurnCaps

	// The fields below are used by [StartStub] only.

	// Resize receives terminal resizes. Nil accepts and discards them.
	Resize func(cols, rows uint16) error
	// PromptInFlight reports whether a turn is open. Nil reports none.
	PromptInFlight func() bool
	// Stop runs when the agent is stopped.
	Stop func()
	// Exited, when closed, ends the stub's process: the agent stops
	// reporting alive, as it does when a provider's output stream closes.
	Exited <-chan struct{}
	// Started receives what Start resolved for this process.
	Started func(StubStart)
}

// StubStart is what [StartStub] resolved before handing the stub its
// process: the same values a provider backend receives.
type StubStart struct {
	Config    Config
	SessionID string
	// JSONLPath is the Claude-shaped transcript path for SessionID.
	JSONLPath string
	// Resuming reports that a transcript already exists at JSONLPath.
	Resuming bool
}

// StartStub starts an Agent through the machinery [Start] uses for a real
// provider (session id and transcript resolution, fail-closed resume, the
// Goal loop, event subscription, readiness) with ops standing in for the
// provider process. It never consults a daemon.
//
// It is the seam for code built on claudia that needs agents without
// provider processes: a test of a host loop, or the claudia daemon's own
// suite, drives the product's Agent rather than a parallel fake that can
// drift from it. Events are delivered with [Agent.PublishEvent].
func StartStub(ctx context.Context, cfg Config, ops *StubAgentOps) (*Agent, error) {
	if ops == nil {
		ops = &StubAgentOps{}
	}
	return startWithBackendContext(ctx, cfg, &stubAgentBackend{ops: ops})
}

type stubAgentBackend struct{ ops *StubAgentOps }

func (b *stubAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{Session: true, Resume: true, Rewind: true, TerminalBytes: true}
}

func (b *stubAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	o := b.ops
	if o.Started != nil {
		o.Started(StubStart{Config: req.Config, SessionID: req.SessionID, JSONLPath: req.JSONLPath, Resuming: req.Resuming})
	}
	ctrl := &stubControl{bytes: make(chan []byte)}
	if o.Exited != nil {
		go func() {
			<-o.Exited
			_ = ctrl.Close()
		}()
	}
	ops := agentOps{
		send: func(_ *Agent, text string) error {
			if o.Send == nil {
				return nil
			}
			return o.Send(text)
		},
		resize: func(_ *Agent, cols, rows uint16) error {
			if o.Resize == nil {
				return nil
			}
			return o.Resize(cols, rows)
		},
		stop: func(*Agent) {
			if o.Stop != nil {
				o.Stop()
			}
			_ = ctrl.Close()
		},
		promptInFlight: func(*Agent) bool { return o.PromptInFlight != nil && o.PromptInFlight() },
	}
	if o.Interrupt != nil {
		ops.interrupt = func(*Agent) error { return o.Interrupt() }
	}
	if o.Steer != nil {
		ops.steer = func(_ *Agent, text string) (DeliveryOutcome, error) { return o.Steer(text) }
	}
	if o.TurnPhase != nil {
		ops.turnPhase = func(*Agent) TurnPhase { return o.TurnPhase() }
	}
	if o.TurnCaps != nil {
		ops.turnCaps = func(*Agent) TurnCaps { return o.TurnCaps() }
	}
	return &agentStart{
		Control:     ctrl,
		Ops:         ops,
		DetectReady: func(a *Agent) { close(a.ready) },
	}, nil
}

// stubControl is a stub process's output stream: it carries no bytes and
// closes when the process ends.
type stubControl struct {
	bytes chan []byte
	once  sync.Once
}

func (c *stubControl) Bytes() <-chan []byte { return c.bytes }

func (c *stubControl) Close() error {
	c.once.Do(func() { close(c.bytes) })
	return nil
}

// StubTaskRun is one run a [NewStubTask] is asked to perform.
type StubTaskRun struct {
	Prompt    string
	SessionID string
	Model     string
	WorkDir   string
	// RawLog is the consumer's raw-line func, nil when none was set.
	RawLog RawLogFunc
}

// StubTaskOps stands in for a Task's provider.
type StubTaskOps struct {
	// Run performs one run. The channel carries its events and closes when
	// the run ends. Nil completes immediately with no events.
	Run func(ctx context.Context, run StubTaskRun) (<-chan TaskEvent, error)
	// Interrupt is Task.Cancel. Nil succeeds.
	Interrupt func() error
}

// NewStubTask returns a Task whose runs are ops, through the same Run
// machinery a provider Task uses (status, session and result recording).
// It never consults a daemon.
func NewStubTask(cfg TaskConfig, ops *StubTaskOps) *Task {
	if ops == nil {
		ops = &StubTaskOps{}
	}
	t := newTaskWithBackend(cfg, &stubTaskBackend{ops: ops})
	t.direct = true
	return t
}

type stubTaskBackend struct{ ops *StubTaskOps }

func (b *stubTaskBackend) Capabilities() providerCapabilities {
	return providerCapabilities{Task: true, Resume: true}
}

func (b *stubTaskBackend) RunTask(ctx context.Context, req taskRunRequest) (*taskRun, error) {
	run := StubTaskRun{Prompt: req.Prompt, SessionID: req.SessionID, Model: req.Model, WorkDir: req.WorkDir, RawLog: req.RawLog}
	var events <-chan TaskEvent
	if b.ops.Run != nil {
		ch, err := b.ops.Run(ctx, run)
		if err != nil {
			return nil, err
		}
		events = ch
	} else {
		ch := make(chan TaskEvent)
		close(ch)
		events = ch
	}
	return &taskRun{events: events, interrupt: func() error {
		if b.ops.Interrupt == nil {
			return nil
		}
		return b.ops.Interrupt()
	}}, nil
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

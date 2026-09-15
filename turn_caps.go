// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "context"

// TurnPhase is observed seat state: whether the provider has a turn open
// right now. It is a reading, not a delivery outcome — see
// [DeliveryOutcome] for what a Send/Steer/Interrupt actually did.
type TurnPhase string

const (
	// TurnIdle means no turn is open; a submit starts one.
	TurnIdle TurnPhase = "idle"
	// TurnInTurn means the provider is working a turn; a submit either
	// queues, is rejected, or supersedes, per [TurnCaps].BusyOnSecondSubmit.
	TurnInTurn TurnPhase = "in_turn"
)

// SteerPolicy describes how a provider absorbs steer text when it
// supports [Agent.Steer] at all.
type SteerPolicy string

const (
	// SteerBreakpoint folds the text in at the agent's next breakpoint
	// (Cursor ACP: a second session/prompt supersedes the in-flight one).
	SteerBreakpoint SteerPolicy = "breakpoint"
	// SteerFinishSlice lets the current slice of work finish, then reads
	// the text (Grok ACP; Codex turn/steer).
	SteerFinishSlice SteerPolicy = "finish_slice"
	// SteerQueueUntilIdle is the honest fallback: there is no steer
	// mechanism, the text waits for the turn boundary (Claude tmux).
	SteerQueueUntilIdle SteerPolicy = "queue_until_idle"
	// SteerNone means the provider has no turn to steer (Task-only).
	SteerNone SteerPolicy = "none"
)

// BusyOnSecondSubmit values: what a provider does with a second plain
// submit while [TurnInTurn], observed rather than promised. The field is
// a plain string so the broker wire can carry it verbatim.
const (
	// BusySubmitReject: the provider refuses the second submit (ACP
	// "prompt already in flight", Codex "turn already in flight").
	BusySubmitReject = "reject"
	// BusySubmitSupersede: the provider drops the in-flight turn in favour
	// of the new text.
	BusySubmitSupersede = "supersede"
	// BusySubmitQueue: the provider holds the text until the turn ends
	// (Claude Code's queue-operation records).
	BusySubmitQueue = "queue"
)

// TurnCaps reports what a seat can do to an open turn. Read it from
// [Agent.TurnCaps] for a live handle (which knows what is actually wired)
// or [ProviderTurnCaps] for the provider's contract in the abstract.
type TurnCaps struct {
	// CanInterrupt: [Agent.Interrupt] reaches a provider hard-stop.
	CanInterrupt bool
	// CanSteer: [Agent.Steer] reaches a provider steer mechanism. False
	// means Steer returns [ErrSteerUnsupported]; the caller queues or
	// interrupts and resubmits.
	CanSteer bool
	// SteerPolicy is how steer text is absorbed when CanSteer, and the
	// honest fallback label when it is not.
	SteerPolicy SteerPolicy
	// BusyOnSecondSubmit: what the provider does if a second submit
	// arrives while in_turn without an explicit Steer call. One of
	// [BusySubmitReject], [BusySubmitSupersede], [BusySubmitQueue]; empty
	// for Task-only providers, which have no turn to be busy in.
	BusyOnSecondSubmit string
}

// providerTurnCaps is the design's backend mapping
// (docs/design/steer-interrupt-turn-api.md, "Backend mapping"): the
// provider's contract, before asking whether this build has wired it.
// TestProviderTurnCapsIsTotal keeps it total over the provider set.
var providerTurnCaps = map[Provider]TurnCaps{
	// ESC is the hard stop. Claude Code exposes no composer-inject
	// while a turn runs; a second submit lands in its queue-operation
	// log and is replayed at the turn boundary.
	ProviderClaude: {
		CanInterrupt:       true,
		CanSteer:           false,
		SteerPolicy:        SteerQueueUntilIdle,
		BusyOnSecondSubmit: BusySubmitQueue,
	},
	// session/cancel hard-stops; a second session/prompt supersedes the
	// in-flight RPC and the merged turn continues (🎯T72.1).
	ProviderCursor: {
		CanInterrupt:       true,
		CanSteer:           true,
		SteerPolicy:        SteerBreakpoint,
		BusyOnSecondSubmit: BusySubmitReject,
	},
	// session/cancel hard-stops; a second session/prompt is read once the
	// current slice finishes (🎯T72.1).
	ProviderGrok: {
		CanInterrupt:       true,
		CanSteer:           true,
		SteerPolicy:        SteerFinishSlice,
		BusyOnSecondSubmit: BusySubmitReject,
	},
	// turn/interrupt hard-stops; turn/steer exists on CLIs that list it
	// in their app-server schema. This is the contract when it does —
	// [Agent.TurnCaps] downgrades to queue_until_idle on a CLI without it.
	ProviderCodex: {
		CanInterrupt:       true,
		CanSteer:           true,
		SteerPolicy:        SteerFinishSlice,
		BusyOnSecondSubmit: BusySubmitReject,
	},
	// Task-only: one-shot ConverseStream, nothing to interrupt or steer.
	ProviderBedrock: {SteerPolicy: SteerNone},
	// Task-only: one-shot generate, nothing to interrupt or steer.
	ProviderOllama: {SteerPolicy: SteerNone},
}

// ProviderTurnCaps returns provider's turn-control contract as the design
// maps it. An empty Provider means [ProviderClaude]; an unknown provider
// reports nothing supported. A live handle's [Agent.TurnCaps] is the
// authoritative answer — it also knows whether the mechanism is wired
// for that handle.
func ProviderTurnCaps(provider Provider) TurnCaps {
	if provider == "" {
		provider = ProviderClaude
	}
	if caps, ok := providerTurnCaps[provider]; ok {
		return caps
	}
	return TurnCaps{SteerPolicy: SteerNone}
}

// withoutSteer is caps with the steer claim withdrawn: the honest label
// for a handle whose provider could steer but whose mechanism is not
// wired (ACP seam not yet landed, Codex CLI without turn/steer).
func (c TurnCaps) withoutSteer() TurnCaps {
	c.CanSteer = false
	if c.SteerPolicy != SteerNone {
		c.SteerPolicy = SteerQueueUntilIdle
	}
	return c
}

// turnSteerer is the seam a provider client satisfies to back
// [Agent.Steer]. The ACP clients (cursor_acp.go, grok_acp.go — 🎯T72.1)
// and the Codex app-server client (codex_steer.go) implement it; agent.go
// wires it into agentOps through [steerOp].
//
// Steer folds text into the running turn and returns the mechanism that
// ran (a [DeliveryOutcome].Mechanism label). SupersededTurnID is the turn
// id the last Steer pushed over, or "" when the mechanism does not
// supersede.
type turnSteerer interface {
	Steer(ctx context.Context, text string) (mechanism string, err error)
	SupersededTurnID() string
}

// steerOp adapts a provider client into the agentOps.steer hook. It takes
// `any` on purpose: a client that does not (yet) implement [turnSteerer]
// yields a nil hook, so [Agent.Steer] reports [ErrSteerUnsupported] and
// [Agent.TurnCaps] withdraws the steer claim — and the moment the client
// grows the methods, the same wiring line lights up without an edit here.
func steerOp(client any) func(*Agent, string) (DeliveryOutcome, error) {
	s, ok := client.(turnSteerer)
	if !ok || s == nil {
		return nil
	}
	return func(_ *Agent, text string) (DeliveryOutcome, error) {
		mechanism, err := s.Steer(context.Background(), text)
		return DeliveryOutcome{Mechanism: mechanism, SupersededTurnID: s.SupersededTurnID()}, err
	}
}

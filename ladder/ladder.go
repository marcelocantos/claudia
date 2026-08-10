// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"context"
	"errors"
	"fmt"
)

// Note is something a layer added to a request on its way up. A layer
// that cannot answer is not thereby useless: it may resolve a reference,
// flag an ambiguity, or assemble context, and then escalate carrying
// what it added.
//
// Notes are the plain form of that. The reserved out-of-band channel
// that carries uncertainty and provenance through a model round trip is
// a separate concern; this is the in-process equivalent.
type Note struct {
	// Layer is which rung added this.
	Layer string
	// Kind classifies the note — a consumer's vocabulary, not
	// claudia's. Typical: "resolution", "uncertainty", "staleness".
	Kind string
	// Text is the note itself.
	Text string
	// Value carries structured detail where the consumer wants it.
	Value any
}

// Request walks the ladder. Layers read it, may append notes to it, and
// either answer it or escalate.
type Request struct {
	// Kind is the consumer's request class. It is opaque to claudia and
	// is what a consumer keys its own routing and evidence on.
	Kind string

	// Payload is the request itself, in whatever shape the consumer
	// uses.
	Payload any

	// Notes accumulate as the request climbs. A layer appends with
	// [Request.Note].
	Notes []Note
}

// Note appends a note to the request. It is how a layer contributes
// without answering: contributing and escalating are not exclusive.
func (r *Request) Note(n Note) {
	r.Notes = append(r.Notes, n)
}

// Layer is one rung. A rule, a local model and a frontier model all
// satisfy this, which is the substitutability the whole design rests on:
// if they did not, the ladder would be several bespoke integrations
// wearing a shared name.
type Layer interface {
	// Scope is the layer's resolved manifest. The ladder uses it to
	// build the layer's reader and to validate what its verdicts name.
	Scope() *Scope

	// Evaluate answers the request or abstains. It must return a
	// non-nil verdict or a non-nil error; returning neither is the one
	// forbidden outcome, because a layer that passes through silently
	// leaves nothing to inspect and nothing to demote on.
	//
	// Evaluate must not perform effects. It names an action in its
	// verdict and the ladder performs it, which keeps the classifier
	// replayable.
	Evaluate(ctx context.Context, req *Request, rd *Reader) (*Verdict, error)
}

// Rung is what one layer did with a request: it answered, it abstained,
// or it failed. Every rung a request touches produces one of these, so a
// request's history is complete by construction.
type Rung struct {
	Layer   string
	Verdict *Verdict
	// Err is set when the layer failed. A failure escalates — expensive
	// but correct — rather than dropping the request.
	Err error
	// Defect is set when the layer broke the contract, which is
	// reported rather than tolerated.
	Defect string
	// Reads is what the layer looked up, in order, so the rung replays.
	Reads []ReadRecord
}

// Result is the outcome of one walk.
type Result struct {
	// Verdict is the answer, and Layer is which rung produced it.
	Verdict *Verdict
	Layer   string

	// Output is what the action returned, for an acting verdict.
	Output any

	// Rungs is every layer the request touched, in order — including
	// the ones that abstained or failed. This is the artifact that
	// makes a wrong cheap answer inspectable instead of invisible.
	Rungs []Rung
}

// Escalated reports whether the request passed at least one rung before
// being answered. A consumer meters this: a collapse toward zero
// escalations is indistinguishable from the ladder optimising away its
// own oversight, and is a defect signal until proven otherwise.
func (r *Result) Escalated() bool {
	return len(r.Rungs) > 1
}

// ErrNoLayerAnswered means the request reached the top of the ladder
// unanswered.
//
// This is a configuration fault, not a decision: an abstention with no
// rung above it must escalate to a model, so a ladder whose top rung can
// abstain is missing its top rung. Failing here is deliberate — the
// alternative is a lower rung's abstention quietly becoming an answer.
var ErrNoLayerAnswered = errors.New("ladder: every layer abstained and no rung above remains")

// Ladder is an ordered set of layers, cheapest first.
type Ladder struct {
	layers []Layer
}

// New returns a ladder over the given layers, in the order they will be
// consulted. A ladder with no layers is valid and answers nothing, which
// is how a consumer that has registered no layers keeps today's
// behaviour.
func New(layers ...Layer) *Ladder {
	return &Ladder{layers: layers}
}

// Evaluate walks the ladder until a layer answers.
//
// Every rung is recorded whatever it did. A layer that errors escalates
// rather than failing the request: degrading toward the expensive tier
// is correct, and degrading toward dropping the request never is. A
// layer that returns neither verdict nor error has broken the contract,
// and that is recorded as a defect on its rung rather than being
// smoothed over.
func (l *Ladder) Evaluate(ctx context.Context, req *Request) (*Result, error) {
	if req == nil {
		return nil, fmt.Errorf("ladder: nil Request")
	}
	res := &Result{}

	for _, layer := range l.layers {
		scope := layer.Scope()
		if scope == nil {
			res.Rungs = append(res.Rungs, Rung{Defect: "layer has no scope"})
			continue
		}

		rd := scope.NewReader()
		verdict, err := layer.Evaluate(ctx, req, rd)
		rung := Rung{Layer: scope.Layer(), Reads: rd.Records()}

		switch {
		case err != nil:
			// Fail upward. The request keeps climbing.
			rung.Err = err
		case verdict == nil:
			rung.Defect = "layer returned neither a verdict nor an error"
		default:
			rung.Verdict = verdict
			if verdict.Layer == "" {
				verdict.Layer = scope.Layer()
			}
		}
		res.Rungs = append(res.Rungs, rung)

		if rung.Verdict == nil || rung.Verdict.Kind == VerdictAbstain {
			continue
		}

		if err := rung.Verdict.Validate(scope); err != nil {
			// A verdict outside the layer's manifest is refused with
			// nothing performed, and the request climbs past it.
			res.Rungs[len(res.Rungs)-1].Err = err
			res.Rungs[len(res.Rungs)-1].Verdict = nil
			continue
		}

		res.Verdict = rung.Verdict
		res.Layer = rung.Verdict.Layer
		if rung.Verdict.Kind == VerdictAct {
			out, err := scope.Perform(ctx, rung.Verdict)
			if err != nil {
				return res, fmt.Errorf("ladder: layer %q performing %q: %w", scope.Layer(), rung.Verdict.Action.Name(), err)
			}
			res.Output = out
		}
		return res, nil
	}

	return res, ErrNoLayerAnswered
}

// LayerFunc adapts a function to the [Layer] interface, for layers whose
// whole implementation is one classifier.
type LayerFunc struct {
	scope *Scope
	fn    func(ctx context.Context, req *Request, rd *Reader) (*Verdict, error)
}

// NewLayerFunc returns a Layer backed by fn.
func NewLayerFunc(scope *Scope, fn func(ctx context.Context, req *Request, rd *Reader) (*Verdict, error)) *LayerFunc {
	return &LayerFunc{scope: scope, fn: fn}
}

// Scope implements [Layer].
func (l *LayerFunc) Scope() *Scope { return l.scope }

// Evaluate implements [Layer].
func (l *LayerFunc) Evaluate(ctx context.Context, req *Request, rd *Reader) (*Verdict, error) {
	return l.fn(ctx, req, rd)
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package laddertest is the conformance harness every rung is held to.
//
// Substitutability is the property the whole tiered design rests on: a
// pattern rule, a local model and a frontier model must satisfy one
// contract, or the ladder is several bespoke integrations wearing a
// shared name. That claim is worth nothing asserted in documentation, so
// it is a suite instead — run the same checks against each rung,
// including rungs a consumer wrote.
package laddertest

import (
	"context"
	"fmt"

	"github.com/marcelocantos/claudia/ladder"
)

// TB is the slice of *testing.T the harness needs. It is an interface
// rather than testing.TB so the harness can be tested against a layer
// that is deliberately wrong.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// Case is one request to put through a layer. Cases are the consumer's:
// claudia has no idea what its layers decide about.
type Case struct {
	Name    string
	Request *ladder.Request
}

// Config configures a conformance run.
type Config struct {
	// Layer under test.
	Layer ladder.Layer

	// Cases to evaluate. Include at least one the layer answers and one
	// it abstains on; a suite where the layer abstains on everything
	// passes trivially and proves nothing.
	Cases []Case
}

// Conform checks a layer against the ladder contract.
//
// What it verifies, and why each one matters:
//
//   - Evaluate returns a verdict or an error, never neither. Silent
//     pass-through is the single forbidden outcome, because it leaves
//     no artifact to inspect and nothing to demote on.
//   - A verdict validates against the layer's own scope, so a layer
//     cannot name an action its manifest does not cover.
//   - An abstaining or answering verdict names no action.
//   - Evaluate does not panic. A layer that panics would take the
//     request down with it instead of escalating.
//   - The layer is deterministic under replay: re-evaluating the same
//     request against the recorded reads reaches the same verdict. This
//     is what the replay oracle depends on, and a layer that consults
//     the clock or the network directly fails here.
//
// What it cannot verify, and a consumer must: that Evaluate performs no
// effects. Purity of that kind is not observable from outside the layer;
// keeping tool access in the actuator is the discipline that supplies it.
func Conform(t TB, cfg *Config) {
	t.Helper()

	if cfg == nil || cfg.Layer == nil {
		t.Errorf("laddertest: no layer under test")
		return
	}
	scope := cfg.Layer.Scope()
	if scope == nil {
		t.Errorf("laddertest: layer has no scope")
		return
	}
	if len(cfg.Cases) == 0 {
		t.Errorf("laddertest: no cases; a layer proves nothing against an empty suite")
		return
	}

	for _, c := range cfg.Cases {
		conformCase(t, scope, cfg.Layer, c)
	}
}

func conformCase(t TB, scope *ladder.Scope, layer ladder.Layer, c Case) {
	t.Helper()

	if c.Request == nil {
		t.Errorf("%s: nil request", c.Name)
		return
	}

	ctx := context.Background()
	rd := scope.NewReader()

	verdict, err, panicked := evaluateSafely(ctx, layer, c.Request, rd)
	if panicked {
		t.Errorf("%s: layer panicked (%v); a rung must escalate, not take the request down with it", c.Name, err)
		return
	}
	if err != nil && verdict != nil {
		t.Errorf("%s: returned both a verdict and an error", c.Name)
	}
	if err == nil && verdict == nil {
		t.Errorf("%s: returned neither a verdict nor an error — silent pass-through is the one forbidden outcome", c.Name)
		return
	}
	if err != nil {
		// Failing is allowed; the ladder escalates past it. Nothing
		// further to check on this case.
		return
	}

	if vErr := verdict.Validate(scope); vErr != nil {
		t.Errorf("%s: verdict does not satisfy the layer's own scope: %v", c.Name, vErr)
		return
	}

	switch verdict.Kind {
	case ladder.VerdictAbstain, ladder.VerdictAnswer:
		if !verdict.Action.IsZero() {
			t.Errorf("%s: %s verdict names an action", c.Name, verdict.Kind)
		}
	case ladder.VerdictAct:
		if verdict.Action.IsZero() {
			t.Errorf("%s: acting verdict names no action", c.Name)
		}
	default:
		t.Errorf("%s: unknown verdict kind %q", c.Name, verdict.Kind)
	}

	// Determinism under replay. The recorded reads are served back and
	// the layer must reach the same verdict; anything consulting a
	// clock, a random source or the network directly diverges here.
	replayReq := &ladder.Request{Kind: c.Request.Kind, Payload: c.Request.Payload}
	replayRd := scope.NewReplayReader(rd.Records())
	replayVerdict, replayErr, replayPanicked := evaluateSafely(ctx, layer, replayReq, replayRd)
	switch {
	case replayPanicked:
		t.Errorf("%s: layer panicked on replay: %v", c.Name, replayErr)
	case replayErr != nil:
		t.Errorf("%s: replay failed where the live run succeeded: %v", c.Name, replayErr)
	case replayVerdict == nil:
		t.Errorf("%s: replay returned no verdict where the live run did", c.Name)
	default:
		if replayVerdict.Kind != verdict.Kind {
			t.Errorf("%s: replay verdict kind %q, live %q", c.Name, replayVerdict.Kind, verdict.Kind)
		}
		if replayVerdict.Rule != verdict.Rule {
			t.Errorf("%s: replay rule %q, live %q", c.Name, replayVerdict.Rule, verdict.Rule)
		}
		if replayVerdict.Action.Name() != verdict.Action.Name() {
			t.Errorf("%s: replay named action %q, live %q", c.Name, replayVerdict.Action.Name(), verdict.Action.Name())
		}
	}
}

// evaluateSafely catches a panicking layer so the harness reports it as
// a contract failure rather than dying with it. The panic is reported
// separately from an ordinary error, because returning an error is
// allowed — the ladder escalates past it — and panicking is not.
func evaluateSafely(ctx context.Context, layer ladder.Layer, req *ladder.Request, rd *ladder.Reader) (v *ladder.Verdict, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			v, err, panicked = nil, fmt.Errorf("%v", r), true
		}
	}()
	v, err = layer.Evaluate(ctx, req, rd)
	return v, err, false
}

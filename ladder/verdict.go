// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import "fmt"

// VerdictKind is what a layer decided to do about a request.
type VerdictKind string

const (
	// VerdictAbstain means the layer has no opinion and the request
	// walks to the next rung. Abstention is a first-class verdict, not
	// a failure: it is how a cheap layer declines without denying.
	//
	// With no reachable rung above, an abstention escalates to a model.
	// A disabled upper rung never becomes a licence to decide locally.
	VerdictAbstain VerdictKind = "abstain"

	// VerdictAnswer means the layer answered without needing an effect.
	VerdictAnswer VerdictKind = "answer"

	// VerdictAct means the layer named an action it wants performed. It
	// did not perform one: [Scope.Perform] is the only path to an
	// effect, and it validates the named action against the layer's
	// scope first.
	VerdictAct VerdictKind = "act"
)

// Verdict is what a layer returns. It is declarative — a layer says what
// it decided and, where an effect is wanted, which action it wants —
// which is what keeps a classifier pure and replayable while leaving the
// actuator as the single place authority is checked.
//
// A verdict is also the unit of replay. Replaying a recorded request
// replays the verdict; it never re-runs the effect.
type Verdict struct {
	Kind VerdictKind

	// Layer and Rule are provenance: which layer answered, and under
	// which rule. They are what let a consumer distinguish "the model
	// said X" from "a rule said X", which warrant different trust.
	Layer string
	Rule  string

	// Reason is the prose a deterministic answer explains itself with.
	// Without it, every audit question costs exactly what the rule
	// saved, and the cheapest actions become the least accountable.
	Reason string

	// Answer carries the response for VerdictAnswer.
	Answer any

	// Action and Args carry the request for VerdictAct.
	Action Action
	Args   any
}

// Validate checks a verdict against the scope of the layer that produced
// it. It is called by [Scope.Perform] before anything executes, so a
// layer naming an action outside its manifest is refused with nothing
// having happened.
func (v *Verdict) Validate(s *Scope) error {
	if v == nil {
		return fmt.Errorf("ladder: nil Verdict")
	}
	if s == nil {
		return fmt.Errorf("ladder: nil Scope")
	}

	switch v.Kind {
	case VerdictAbstain:
		if !v.Action.IsZero() {
			return fmt.Errorf("ladder: abstaining verdict must not name an action")
		}
	case VerdictAnswer:
		if !v.Action.IsZero() {
			return fmt.Errorf("ladder: answering verdict must not name an action")
		}
	case VerdictAct:
		if v.Action.IsZero() {
			return fmt.Errorf("ladder: acting verdict names no action")
		}
		if !s.AllowsAction(v.Action) {
			return &ScopeError{Layer: s.layer, Kind: "action", Name: v.Action.name}
		}
	default:
		return fmt.Errorf("ladder: unknown verdict kind %q", v.Kind)
	}
	return nil
}

// Explain renders why this verdict happened, with no model involved. It
// prefers the verdict's own reason and falls back to the prose the
// action was registered with.
func (v *Verdict) Explain(s *Scope) string {
	who := v.Layer
	if who == "" && s != nil {
		who = s.layer
	}
	if who == "" {
		who = "unknown layer"
	}

	what := v.Reason
	if what == "" && v.Kind == VerdictAct && s != nil {
		if desc, err := s.reg.DescribeAction(v.Action); err == nil {
			what = desc
		}
	}
	if what == "" {
		what = "no reason recorded"
	}

	if v.Rule != "" {
		return fmt.Sprintf("%s (%s): %s [%s]", who, v.Rule, what, v.Kind)
	}
	return fmt.Sprintf("%s: %s [%s]", who, what, v.Kind)
}

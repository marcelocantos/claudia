// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

// Candidate is something a consolidation pass thinks should change. It
// is a PROPOSAL and nothing more: the pass that reasons is not the pass
// that acts.
type Candidate struct {
	// Kind is promote, demote or revoke.
	Kind string

	RuleID string
	Class  string

	// Why states the finding in prose, and becomes the rule's
	// description if promoted — which is what lets a deterministic
	// answer explain itself later with no model involved.
	Why string

	// Evidence pooled across every stack that contributed.
	Evidence Evidence

	// PerStack is the same evidence broken down by contributor.
	//
	// A rule can look excellent on the pool and be wrong for every
	// member; this is what makes that visible rather than assumed.
	PerStack map[string]Evidence

	// HoldsEverywhere reports whether the candidate meets the gate on
	// EVERY contributing stack, not merely in aggregate.
	HoldsEverywhere bool

	// Blocked names why a candidate cannot be raised as a proposal —
	// most often because it would exceed the scope it must run under.
	Blocked string
}

// Candidate kinds.
const (
	CandidatePromote = "promote"
	CandidateDemote  = "demote"
	CandidateRevoke  = "revoke"
)

// ConsolidationPass configures [Consolidate].
type ConsolidationPass struct {
	// Rules being revised.
	Rules *RuleSet

	// Journal supplies the episodes to reason over.
	Journal Journal

	// Scope a candidate must be able to run under. A candidate naming
	// an action outside it is blocked rather than proposed.
	Scope *Scope

	// ModelLayers names the model-backed rungs.
	//
	// It is REQUIRED, because only a class a model is still answering is
	// worth crystallising, and claudia cannot infer which rungs are
	// models — a rung is an interface, and what sits behind it is the
	// consumer's business. An earlier version guessed by testing whether
	// the answering verdict carried a rule id, which is wrong the moment
	// a model rung stamps one.
	ModelLayers []string

	// Thresholds gate promotion. Nil uses [ProductionThresholds].
	Thresholds *Thresholds

	// ActionsOf reports which actions a candidate rule would name, so
	// the pass can check it against Scope. Claudia does not interpret
	// rule bodies.
	ActionsOf func(ruleID string) []string

	// Symptoms are findings from [Scan], turned into demotion and
	// revocation candidates. A pass that only ever adds rules makes the
	// store monotonically more rigid.
	Symptoms []Symptom
}

// Consolidate reads episodes and proposes changes. It INSTALLS NOTHING.
//
// The separation between reasoning and acting is a fact about the
// signature rather than a discipline someone must remember: Consolidate
// cannot install because it returns candidates, and [Store.Install]
// refuses a proposal raised in the pass that is installing it. The two
// mechanisms agree by construction.
//
// This runs offline, when the consumer schedules it. Consolidating
// inside the decision being generalised is precisely what the
// motor-learning constraint forbids — you do not automatise
// mid-performance.
func Consolidate(ctx context.Context, pass *ConsolidationPass) ([]Candidate, error) {
	switch {
	case pass == nil:
		return nil, fmt.Errorf("ladder: nil ConsolidationPass")
	case pass.Rules == nil:
		return nil, fmt.Errorf("ladder: consolidation needs a rule set")
	case pass.Journal == nil:
		return nil, fmt.Errorf("ladder: consolidation needs a journal — there is nothing to learn from without episodes")
	case pass.Scope == nil:
		return nil, fmt.Errorf("ladder: consolidation needs the scope candidates must run under")
	case len(pass.ModelLayers) == 0:
		return nil, fmt.Errorf("ladder: consolidation needs ModelLayers — only a class a model still answers is worth crystallising, and claudia cannot infer which rungs are models")
	}

	thresholds := pass.Thresholds
	if thresholds == nil {
		thresholds = ProductionThresholds()
	}

	fingerprint, err := pass.Rules.Fingerprint()
	if err != nil {
		return nil, err
	}

	// Pool by decision class, and keep the per-stack breakdown that
	// makes the pooled figure checkable.
	pooled := make(map[string]*Evidence)
	perStack := make(map[string]map[string]Evidence)
	answeredByModel := make(map[string]int)

	for _, ep := range pass.Journal.Episodes() {
		// Episodes from a different rule set describe a world that no
		// longer exists.
		if ep.Fingerprint != "" && ep.Fingerprint != fingerprint {
			continue
		}
		if ep.Outcome == nil {
			// No judgement is no evidence. It is not counted as
			// favourable, merely uncounted.
			continue
		}

		e, ok := pooled[ep.Kind]
		if !ok {
			e = &Evidence{TestsPass: true}
			pooled[ep.Kind] = e
			perStack[ep.Kind] = make(map[string]Evidence)
		}
		e.Runs++
		if ep.Outcome.Delivered {
			e.Identical++
			e.Corrections++
		} else {
			e.Corrections++
			e.CorrectionsAgainst++
		}
		if ep.Outcome.Correction {
			e.CorrectionsAgainst++
		}
		if slices.Contains(pass.ModelLayers, ep.AnsweredBy) {
			answeredByModel[ep.Kind]++
		}

		s := perStack[ep.Kind][ep.Stack]
		s.TestsPass = true
		s.Runs++
		if ep.Outcome.Delivered {
			s.Identical++
		}
		perStack[ep.Kind][ep.Stack] = s
	}

	var out []Candidate

	classes := make([]string, 0, len(pooled))
	for class := range pooled {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	for _, class := range classes {
		ev := *pooled[class]
		// Only a class a model is still handling is worth crystallising.
		if answeredByModel[class] == 0 {
			continue
		}
		if ev.Runs < thresholds.AgentToHybridRuns {
			continue
		}
		if ev.Consistency() < thresholds.AgentToHybridConsistency {
			continue
		}

		c := Candidate{
			Kind:     CandidatePromote,
			RuleID:   "learned-" + class,
			Class:    class,
			Why:      fmt.Sprintf("%s delivered on %d of %d episodes across %d stacks", class, ev.Identical, ev.Runs, len(perStack[class])),
			Evidence: ev,
			PerStack: perStack[class],
		}

		// The pooled figure is not enough. A rule that holds only in
		// aggregate is flagged rather than proposed.
		c.HoldsEverywhere = true
		for _, s := range perStack[class] {
			if s.Runs == 0 || s.Consistency() < thresholds.AgentToHybridConsistency {
				c.HoldsEverywhere = false
				break
			}
		}
		if !c.HoldsEverywhere {
			c.Blocked = "holds on the pool but not on every contributing stack; the flavour may not be homogeneous"
		}

		// A candidate that cannot pass the load-time gate is not a
		// candidate.
		if pass.ActionsOf != nil {
			for _, name := range pass.ActionsOf(c.RuleID) {
				if _, err := pass.Scope.LookupAction(name); err != nil {
					c.Blocked = fmt.Sprintf("names %q, which layer %q may not perform", name, pass.Scope.Layer())
					break
				}
			}
		}
		out = append(out, c)
	}

	// Symptoms become demotions and revocations, so a pass does not only
	// ever add. A store that grows monotonically becomes monotonically
	// more rigid, which is the failure the motor-learning frame predicts.
	for _, s := range pass.Symptoms {
		switch s.Kind {
		case SymptomInert, SymptomNeverFired:
			out = append(out, Candidate{
				Kind: CandidateRevoke, RuleID: s.RuleID, Class: s.Class, Why: s.Detail,
			})
		case SymptomContradicted:
			out = append(out, Candidate{
				Kind: CandidateDemote, RuleID: s.RuleID, Class: s.Class, Why: s.Detail,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out, nil
}

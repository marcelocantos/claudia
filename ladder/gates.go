// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"errors"
	"fmt"
)

// Stage is how far a decision class has crystallised. Autonomy attaches
// to the class on its evidence, not to the capability of whatever model
// happens to be behind it.
type Stage string

const (
	// StageAgent means a model decides.
	StageAgent Stage = "agent"
	// StageHybrid means a cheaper model decides, with the expensive one
	// as the escalation path. It is a safer resting place than a rule,
	// because a cheap model still generalises where a rule cannot.
	StageHybrid Stage = "hybrid"
	// StageDeterministic means a rule decides.
	StageDeterministic Stage = "deterministic"
)

// Thresholds gate promotion. They are CONFIGURATION, not constants:
// claudia has no opinion about how much evidence is enough, because that
// depends on what the decision costs when it is wrong, which is the
// consumer's to know.
//
// Sensible starting values, from a production system that ran this
// architecture for eight months, are in [ProductionThresholds].
type Thresholds struct {
	AgentToHybridRuns        int
	AgentToHybridConsistency float64

	HybridToDeterministicRuns        int
	HybridToDeterministicConsistency float64
}

// ProductionThresholds are the gates reported by Progressive
// Crystallization (arXiv 2607.07052). They are offered rather than
// imposed: a consumer sets its own.
func ProductionThresholds() *Thresholds {
	return &Thresholds{
		AgentToHybridRuns:                10,
		AgentToHybridConsistency:         0.90,
		HybridToDeterministicRuns:        50,
		HybridToDeterministicConsistency: 0.99,
	}
}

// Evidence is what has been observed about one decision class.
//
// Consistency proves a rule STABLE, never RIGHT. A systematically wrong
// resolution promotes just as smoothly as a correct one, so a store that
// promoted on consistency alone would be laundering repetition into
// confidence. [Evidence.Corrections] is where a consumer supplies the
// signal consistency cannot: something that judged the outcome.
type Evidence struct {
	// Runs observed for this class.
	Runs int
	// Identical is how many produced the same action sequence.
	Identical int
	// SafetyViolations observed. Any at all blocks promotion.
	SafetyViolations int
	// TestsPass reports whether the rule's own acceptance tests pass.
	TestsPass bool
	// Corrections is external correctness signal — a critic's verdict,
	// a human's correction, downstream behaviour confirming a marked
	// guess. Zero means none was supplied, which is recorded rather
	// than assumed favourable.
	Corrections int
	// CorrectionsAgainst is how many of those said the decision was
	// wrong.
	CorrectionsAgainst int
}

// Consistency is the share of runs that agreed. It reports 0 for no
// runs rather than dividing by zero into a perfect score.
func (e *Evidence) Consistency() float64 {
	if e.Runs == 0 {
		return 0
	}
	return float64(e.Identical) / float64(e.Runs)
}

// ConsistencyOnly reports whether this evidence rests on repetition with
// no correctness signal behind it. Promotion on that basis is permitted
// — a consumer may have nothing better — but it is never invisible.
func (e *Evidence) ConsistencyOnly() bool { return e.Corrections == 0 }

// Proposal is a rule someone thinks should exist. Proposing is not
// installing.
type Proposal struct {
	ID    string
	Class string

	// Description is the situation the rule handles, in prose,
	// requested at mint time so the rule is retrievable by what it is
	// FOR rather than by an exhaustive predicate.
	Description string

	// Rule is the payload. Claudia does not interpret it; the layer
	// that will run it does.
	Rule any

	// ProposedBy identifies the proposer, which is checked against the
	// installer.
	ProposedBy string

	// Pass is the consolidation pass the proposal was raised in.
	Pass string

	Evidence Evidence
}

// Errors the store returns for the gates that matter. They are typed so
// a consumer can tell a refused install from a broken one.
var (
	// ErrSelfInstall reports an install by the proposer. The cheapest
	// expressible rule is "handle everything, escalate nothing", and
	// optimising cost finds it immediately — so the party whose cost a
	// rule reduces does not get to install it.
	ErrSelfInstall = errors.New("ladder: a proposal may not be installed by its proposer")

	// ErrSamePass reports an install in the pass that raised the
	// proposal. The separation that makes self-evolution safe is
	// temporal: propose while working, install while consolidating,
	// with the record present and none of the pressure that produced
	// the proposal.
	ErrSamePass = errors.New("ladder: a proposal may not be installed in the pass that raised it")

	// ErrInsufficientEvidence reports a promotion below threshold.
	ErrInsufficientEvidence = errors.New("ladder: evidence does not meet the threshold for this transition")
)

func (rs *RuleSet) gate(e Evidence, stage Stage) error {
	if e.SafetyViolations > 0 {
		return fmt.Errorf("%w: %d safety violations", ErrInsufficientEvidence, e.SafetyViolations)
	}
	if !e.TestsPass {
		return fmt.Errorf("%w: acceptance tests do not pass", ErrInsufficientEvidence)
	}
	if e.CorrectionsAgainst > 0 {
		return fmt.Errorf("%w: %d corrections say this decision was wrong", ErrInsufficientEvidence, e.CorrectionsAgainst)
	}

	switch stage {
	case StageAgent:
		return nil
	case StageHybrid:
		if e.Runs < rs.thresholds.AgentToHybridRuns {
			return fmt.Errorf("%w: %d runs, want %d", ErrInsufficientEvidence, e.Runs, rs.thresholds.AgentToHybridRuns)
		}
		if e.Consistency() < rs.thresholds.AgentToHybridConsistency {
			return fmt.Errorf("%w: consistency %.2f, want %.2f", ErrInsufficientEvidence, e.Consistency(), rs.thresholds.AgentToHybridConsistency)
		}
	case StageDeterministic:
		if e.Runs < rs.thresholds.HybridToDeterministicRuns {
			return fmt.Errorf("%w: %d runs, want %d", ErrInsufficientEvidence, e.Runs, rs.thresholds.HybridToDeterministicRuns)
		}
		if e.Consistency() < rs.thresholds.HybridToDeterministicConsistency {
			return fmt.Errorf("%w: consistency %.2f, want %.2f", ErrInsufficientEvidence, e.Consistency(), rs.thresholds.HybridToDeterministicConsistency)
		}
	default:
		return fmt.Errorf("ladder: unknown stage %q", stage)
	}
	return nil
}

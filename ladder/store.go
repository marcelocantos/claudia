// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"errors"
	"fmt"
	"sort"
	"sync"
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

// Entry is an installed rule at one point in the store's history.
type Entry struct {
	RuleID      string
	Class       string
	Description string
	Rule        any
	Stage       Stage
	Evidence    Evidence
	// ConsistencyOnly records that this entry was promoted on
	// repetition with no correctness signal, so an audit can find every
	// such rule without recomputing the judgement.
	ConsistencyOnly bool
}

// Version is an immutable snapshot of the whole store. Behaviour is
// history-dependent once rules accumulate, so a replay that does not pin
// a version is silently measuring a moving target.
type Version struct {
	N       int
	Entries []Entry
	// Change describes what produced this version.
	Change string
}

// Lookup returns the entry for a rule, if this version has one.
func (v *Version) Lookup(ruleID string) (Entry, bool) {
	for _, e := range v.Entries {
		if e.RuleID == ruleID {
			return e, true
		}
	}
	return Entry{}, false
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

// Store holds rules, their stage, and the history of how they got
// there.
//
// It is bookkeeping, not policy. It enforces the gates a consumer
// configures and records what happened; it never decides that a rule
// should exist, that one has gone stale, or that a symptom means
// something.
type Store struct {
	thresholds *Thresholds

	mu        sync.RWMutex
	entries   map[string]Entry
	proposals map[string]Proposal
	versions  []*Version
}

// StoreConfig configures a [Store].
type StoreConfig struct {
	// Thresholds gate promotion. Nil uses [ProductionThresholds].
	Thresholds *Thresholds

	// Seed are rules learned in an earlier run of the stack.
	//
	// A seeded store is INDISTINGUISHABLE from one that learned the
	// same rules: restoring is not a special mode and takes no separate
	// code path, so there is no second behaviour to keep in step.
	Seed []Recalled

	// There is deliberately no change callback. Rules move only during a
	// consolidation pass the CONSUMER schedules — that is what the Pass
	// gate on Install enforces — so the consumer already knows when the
	// set changed and can Recall it then. A callback would imply changes
	// arriving at unpredictable moments, which is exactly what offline
	// consolidation rules out.
}

// NewStore returns a store, optionally seeded with rules from an earlier
// run.
//
// It refuses a seed it cannot use rather than starting empty, because an
// empty memory is indistinguishable from one that never learned
// anything, and a ladder in that state quietly reverts to waking a model
// for everything while reporting perfect health.
func NewStore(cfg *StoreConfig) (*Store, error) {
	if cfg == nil {
		cfg = &StoreConfig{}
	}
	thresholds := cfg.Thresholds
	if thresholds == nil {
		thresholds = ProductionThresholds()
	}
	s := &Store{
		thresholds: thresholds,
		entries:    make(map[string]Entry),
		proposals:  make(map[string]Proposal),
	}

	for i, r := range cfg.Seed {
		if r.RuleID == "" {
			return nil, fmt.Errorf("ladder: seed rule %d has no id", i)
		}
		if _, dup := s.entries[r.RuleID]; dup {
			return nil, fmt.Errorf("ladder: seed names rule %q twice", r.RuleID)
		}
		s.entries[r.RuleID] = Entry{
			RuleID:          r.RuleID,
			Class:           r.Class,
			Description:     r.Description,
			Rule:            r.Rule,
			Stage:           r.Stage,
			Evidence:        r.Evidence,
			ConsistencyOnly: r.ConsistencyOnly,
		}
	}

	s.versions = []*Version{{N: 0, Change: "seeded"}}
	if len(cfg.Seed) > 0 {
		s.versions[0].Entries = s.sortedEntries()
	}
	return s, nil
}

// Recall returns the whole rule set in serialisable form, which is what
// a consumer saves.
func (s *Store) Recall() ([]Recalled, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recall()
}

func (s *Store) recall() ([]Recalled, error) {
	out := make([]Recalled, 0, len(s.entries))
	for _, e := range s.sortedEntries() {
		r, err := recalledFrom(e)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// sortedEntries returns the entries in rule-ID order. Caller holds the
// lock.
func (s *Store) sortedEntries() []Entry {
	entries := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RuleID < entries[j].RuleID })
	return entries
}

// Propose records a proposal without installing it.
func (s *Store) Propose(p *Proposal) error {
	if p == nil || p.ID == "" {
		return fmt.Errorf("ladder: proposal needs an ID")
	}
	if p.ProposedBy == "" {
		return fmt.Errorf("ladder: proposal %q needs a proposer", p.ID)
	}
	if p.Pass == "" {
		return fmt.Errorf("ladder: proposal %q needs a pass", p.ID)
	}
	if p.Description == "" {
		return fmt.Errorf("ladder: proposal %q needs a description — a rule nobody can describe cannot explain itself later", p.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.proposals[p.ID]; dup {
		return fmt.Errorf("ladder: proposal %q already raised", p.ID)
	}
	cp := *p
	s.proposals[p.ID] = cp
	return nil
}

// InstallArgs are the parameters of an install.
type InstallArgs struct {
	ProposalID string
	// Installer is who is installing, checked against the proposer.
	Installer string
	// Pass is the consolidation pass doing the installing, checked
	// against the pass that raised the proposal.
	Pass string
	// Stage the rule is being installed at.
	Stage Stage
}

// Install promotes a proposal into the store, subject to every gate.
//
// The gates are deliberately separate checks with distinct errors,
// because they fail for different reasons and a consumer responds to
// them differently.
func (s *Store) Install(args *InstallArgs) (*Version, error) {
	if args == nil {
		return nil, fmt.Errorf("ladder: nil InstallArgs")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.proposals[args.ProposalID]
	if !ok {
		return nil, fmt.Errorf("ladder: no proposal %q", args.ProposalID)
	}
	if args.Installer == "" {
		return nil, fmt.Errorf("ladder: install needs an installer")
	}
	if args.Installer == p.ProposedBy {
		return nil, fmt.Errorf("%w: %q", ErrSelfInstall, p.ProposedBy)
	}
	if args.Pass == "" {
		return nil, fmt.Errorf("ladder: install needs a pass")
	}
	if args.Pass == p.Pass {
		return nil, fmt.Errorf("%w: %q", ErrSamePass, p.Pass)
	}
	if err := s.gate(p.Evidence, args.Stage); err != nil {
		return nil, err
	}

	entry := Entry{
		RuleID:          p.ID,
		Class:           p.Class,
		Description:     p.Description,
		Rule:            p.Rule,
		Stage:           args.Stage,
		Evidence:        p.Evidence,
		ConsistencyOnly: p.Evidence.ConsistencyOnly(),
	}
	s.entries[p.ID] = entry
	delete(s.proposals, p.ID)
	return s.commit(fmt.Sprintf("install %s at %s by %s", p.ID, args.Stage, args.Installer)), nil
}

func (s *Store) gate(e Evidence, stage Stage) error {
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
		if e.Runs < s.thresholds.AgentToHybridRuns {
			return fmt.Errorf("%w: %d runs, want %d", ErrInsufficientEvidence, e.Runs, s.thresholds.AgentToHybridRuns)
		}
		if e.Consistency() < s.thresholds.AgentToHybridConsistency {
			return fmt.Errorf("%w: consistency %.2f, want %.2f", ErrInsufficientEvidence, e.Consistency(), s.thresholds.AgentToHybridConsistency)
		}
	case StageDeterministic:
		if e.Runs < s.thresholds.HybridToDeterministicRuns {
			return fmt.Errorf("%w: %d runs, want %d", ErrInsufficientEvidence, e.Runs, s.thresholds.HybridToDeterministicRuns)
		}
		if e.Consistency() < s.thresholds.HybridToDeterministicConsistency {
			return fmt.Errorf("%w: consistency %.2f, want %.2f", ErrInsufficientEvidence, e.Consistency(), s.thresholds.HybridToDeterministicConsistency)
		}
	default:
		return fmt.Errorf("ladder: unknown stage %q", stage)
	}
	return nil
}

// Demote moves a rule down a stage. It is the circuit breaker: an
// execution failure, a safety violation, an acceptance-test regression
// or an owner's correction takes a rule back toward the model, which is
// expensive and correct.
//
// Demotion needs no evidence bar. Stopping is always allowed.
func (s *Store) Demote(ruleID, reason string) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[ruleID]
	if !ok {
		return nil, fmt.Errorf("ladder: no rule %q", ruleID)
	}
	switch e.Stage {
	case StageDeterministic:
		e.Stage = StageHybrid
	case StageHybrid:
		e.Stage = StageAgent
	case StageAgent:
		return nil, fmt.Errorf("ladder: rule %q is already at %s", ruleID, StageAgent)
	}
	s.entries[ruleID] = e
	return s.commit(fmt.Sprintf("demote %s to %s: %s", ruleID, e.Stage, reason)), nil
}

// Revoke removes a rule.
//
// It costs exactly what installing cost: the same call shape, the same
// version bump, the same audit record, and no extra evidence bar. That
// symmetry is deliberate and is the one place this design refuses to
// imitate its own inspiration — in motor learning unlearning is more
// expensive than learning, and a store that inherited that asymmetry
// would accumulate rigidity by construction.
func (s *Store) Revoke(ruleID, reason string) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.entries[ruleID]; !ok {
		return nil, fmt.Errorf("ladder: no rule %q", ruleID)
	}
	delete(s.entries, ruleID)
	return s.commit(fmt.Sprintf("revoke %s: %s", ruleID, reason)), nil
}

// commit snapshots the current entries as a new immutable version. The
// caller holds the lock.
func (s *Store) commit(change string) *Version {
	v := &Version{N: len(s.versions), Entries: s.sortedEntries(), Change: change}
	s.versions = append(s.versions, v)
	return v
}

// Current returns the newest version.
func (s *Store) Current() *Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.versions[len(s.versions)-1]
}

// Pin returns an immutable earlier version, which is what a replay runs
// against.
func (s *Store) Pin(n int) (*Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n < 0 || n >= len(s.versions) {
		return nil, fmt.Errorf("ladder: no version %d (store has 0..%d)", n, len(s.versions)-1)
	}
	return s.versions[n], nil
}

// Rollback restores the entries of an earlier version as a NEW version.
// History is append-only: rolling back is itself an event, not an
// erasure of the one being undone.
func (s *Store) Rollback(n int) (*Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 || n >= len(s.versions) {
		return nil, fmt.Errorf("ladder: no version %d", n)
	}
	target := s.versions[n]
	s.entries = make(map[string]Entry, len(target.Entries))
	for _, e := range target.Entries {
		s.entries[e.RuleID] = e
	}
	return s.commit(fmt.Sprintf("rollback to version %d", n)), nil
}

// Ablate returns the entries of the current version with one rule
// removed, WITHOUT changing the store.
//
// This is the input to the strongest detector available for a rule that
// has quietly stopped mattering: replay a corpus against this and see
// whether anything changes. You cannot observe the escalation that did
// not happen, but you can observe whether a rule does anything at all.
func (s *Store) Ablate(ruleID string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.RuleID != ruleID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}

// Proposals returns the outstanding proposal IDs, sorted.
func (s *Store) Proposals() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.proposals))
	for id := range s.proposals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// History returns every version in order, which is the audit trail for
// how the store reached its present shape.
func (s *Store) History() []*Version {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Version(nil), s.versions...)
}

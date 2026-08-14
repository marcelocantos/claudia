// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"fmt"
	"sort"
	"sync"
)

// RuleSet is procedural memory for one FLAVOUR of agent: what stacks of
// that kind have learned to do, and the gates that govern learning it.
//
// It is independent of any stack. Several stacks of a flavour attach the
// same set, which is what pools their evidence — a fifty-run promotion
// gate is reached N times faster across N stacks, and a stack running
// alone might never reach it at all.
//
// It is deliberately NOT the journal. Episodes are written on the hot
// path and rules are read there, so one object owning both would put two
// orthogonal accesses behind one lock; and losing a rule set is amnesia
// while losing a journal costs only future opportunity, so a retention
// policy on episodes must never be able to eat rules.
//
// There is ONE rule container. An earlier draft had a separate store for
// learning and a rule set for attaching, and the two halves of the
// evolution loop then took different types — symptoms came out of one
// and candidates went into the other, joined by nothing.
type RuleSet struct {
	// Flavour names the kind of agent these rules belong to. It appears
	// in journal records so pooled evidence stays attributable.
	Flavour string

	thresholds *Thresholds

	mu        sync.RWMutex
	rules     map[string]Recalled
	proposals map[string]Proposal
	history   []Version
}

// RuleSetConfig configures a [RuleSet].
type RuleSetConfig struct {
	// Flavour is required: pooled evidence must stay attributable.
	Flavour string

	// Seed are rules learned in an earlier run of the stack. A seeded
	// set is INDISTINGUISHABLE from one that learned the same rules —
	// restoring takes no separate code path, so there is no second
	// behaviour to keep in step.
	Seed []Recalled

	// Thresholds gate promotion. Nil uses [ProductionThresholds].
	Thresholds *Thresholds
}

// NewRuleSet builds a rule set, optionally seeded from an earlier run.
func NewRuleSet(cfg *RuleSetConfig) (*RuleSet, error) {
	if cfg == nil || cfg.Flavour == "" {
		return nil, fmt.Errorf("ladder: a rule set needs a flavour — pooled evidence must stay attributable")
	}
	thresholds := cfg.Thresholds
	if thresholds == nil {
		thresholds = ProductionThresholds()
	}

	rs := &RuleSet{
		Flavour:    cfg.Flavour,
		thresholds: thresholds,
		rules:      make(map[string]Recalled, len(cfg.Seed)),
		proposals:  make(map[string]Proposal),
	}
	for i, r := range cfg.Seed {
		if r.RuleID == "" {
			return nil, fmt.Errorf("ladder: seed rule %d has no id", i)
		}
		if _, dup := rs.rules[r.RuleID]; dup {
			return nil, fmt.Errorf("ladder: seed names rule %q twice", r.RuleID)
		}
		rs.rules[r.RuleID] = r
	}
	// A seeded set is a version like any other, so a store restored from
	// an earlier run has a pin to hand before it has learned anything.
	if err := rs.commit(VerbSeed, "", "seeded"); err != nil {
		return nil, err
	}
	return rs, nil
}

// LoadRuleSet parses a saved rule set.
//
// It refuses malformed input rather than starting empty, because an
// empty memory is indistinguishable from one that never learned
// anything, and a ladder in that state quietly reverts to waking a model
// for everything while reporting perfect health.
func LoadRuleSet(flavour string, data []byte, thresholds *Thresholds) (*RuleSet, error) {
	rules, err := UnmarshalRules(data)
	if err != nil {
		return nil, err
	}
	return NewRuleSet(&RuleSetConfig{Flavour: flavour, Seed: rules, Thresholds: thresholds})
}

// Save renders the rule set as deterministic YAML for the consumer to
// store. Claudia never writes a file.
func (rs *RuleSet) Save() ([]byte, error) {
	return MarshalRules(rs.Rules())
}

// Rules returns the current rule set, sorted by rule id.
func (rs *RuleSet) Rules() []Recalled {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.sorted()
}

// sorted returns the rules in id order. Caller holds the lock.
func (rs *RuleSet) sorted() []Recalled {
	out := make([]Recalled, 0, len(rs.rules))
	for _, r := range rs.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}

// Lookup returns one rule.
func (rs *RuleSet) Lookup(ruleID string) (Recalled, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	r, ok := rs.rules[ruleID]
	return r, ok
}

// Fingerprint identifies the current contents.
func (rs *RuleSet) Fingerprint() (string, error) {
	return Fingerprint(rs.Rules())
}

// Ablate returns the rule set with one rule removed, WITHOUT changing
// it.
//
// This is the input to the strongest detector available for a rule that
// has quietly stopped mattering: replay a corpus against this and see
// whether anything changes. You cannot observe the escalation that did
// not happen, but you can observe whether a rule does anything at all.
func (rs *RuleSet) Ablate(ruleID string) []Recalled {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	out := make([]Recalled, 0, len(rs.rules))
	for _, r := range rs.sorted() {
		if r.RuleID != ruleID {
			out = append(out, r)
		}
	}
	return out
}

// Propose records a proposal without installing it.
func (rs *RuleSet) Propose(p *Proposal) error {
	switch {
	case p == nil || p.ID == "":
		return fmt.Errorf("ladder: proposal needs an ID")
	case p.ProposedBy == "":
		return fmt.Errorf("ladder: proposal %q needs a proposer", p.ID)
	case p.Pass == "":
		return fmt.Errorf("ladder: proposal %q needs a pass", p.ID)
	case p.Description == "":
		return fmt.Errorf("ladder: proposal %q needs a description — a rule nobody can describe cannot explain itself later", p.ID)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, dup := rs.proposals[p.ID]; dup {
		return fmt.Errorf("ladder: proposal %q already raised", p.ID)
	}
	rs.proposals[p.ID] = *p
	return nil
}

// Proposals returns the outstanding proposal IDs, sorted.
func (rs *RuleSet) Proposals() []string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	ids := make([]string, 0, len(rs.proposals))
	for id := range rs.proposals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
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

// Install promotes a proposal into the rule set, subject to every gate.
//
// The gates are deliberately separate checks with distinct errors,
// because they fail for different reasons and a consumer responds to
// them differently.
func (rs *RuleSet) Install(args *InstallArgs) error {
	if args == nil {
		return fmt.Errorf("ladder: nil InstallArgs")
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	p, ok := rs.proposals[args.ProposalID]
	if !ok {
		return fmt.Errorf("ladder: no proposal %q", args.ProposalID)
	}
	if args.Installer == "" {
		return fmt.Errorf("ladder: install needs an installer")
	}
	if args.Installer == p.ProposedBy {
		return fmt.Errorf("%w: %q", ErrSelfInstall, p.ProposedBy)
	}
	if args.Pass == "" {
		return fmt.Errorf("ladder: install needs a pass")
	}
	if args.Pass == p.Pass {
		return fmt.Errorf("%w: %q", ErrSamePass, p.Pass)
	}
	if err := rs.gate(p.Evidence, args.Stage); err != nil {
		return err
	}

	r, err := recalledFrom(&p, args.Stage)
	if err != nil {
		return err
	}
	rs.rules[p.ID] = r
	delete(rs.proposals, p.ID)
	return rs.commit(VerbInstall, p.ID, fmt.Sprintf("installed at %s by %s in pass %s", args.Stage, args.Installer, args.Pass))
}

// Fail reports a loud failure against a rule that is in force, and
// demotes it.
//
// This is the circuit breaker, and it is bookkeeping rather than policy.
// Claudia does not decide whether a failure was loud — it does not run
// rule bodies and would be guessing — but once a consumer reports one
// under a class the design names, the demotion is automatic: no evidence
// bar, no threshold, no discretion. Stopping is always allowed, so there
// is nothing here for a cost-optimising caller to argue with.
//
// [FailureExternal] is the same act with human provenance, and it takes
// the same path deliberately: an externally supplied demotion that
// behaved differently from an observed one would be a second mechanism to
// keep in step with the first.
func (rs *RuleSet) Fail(ruleID string, f Failure, reason string) (*Demotion, error) {
	if !f.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownFailure, f)
	}
	if reason == "" {
		return nil, fmt.Errorf("ladder: demoting %q needs a reason — a rule that fell for no recorded cause cannot be audited later", ruleID)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.demote(ruleID, f, reason)
}

// Demote moves a rule down a stage on an externally supplied signal —
// an owner's correction, most often, which no amount of observation
// would have produced.
//
// It is [Fail] with [FailureExternal] and nothing else.
func (rs *RuleSet) Demote(ruleID, reason string) error {
	_, err := rs.Fail(ruleID, FailureExternal, reason)
	return err
}

// demote moves one rule down a stage and commits the version. Caller
// holds the write lock.
func (rs *RuleSet) demote(ruleID string, f Failure, reason string) (*Demotion, error) {
	r, ok := rs.rules[ruleID]
	if !ok {
		return nil, fmt.Errorf("ladder: no rule %q", ruleID)
	}
	d := &Demotion{RuleID: ruleID, From: r.Stage, Failure: f, Reason: reason}
	switch r.Stage {
	case StageDeterministic:
		r.Stage = StageHybrid
	case StageHybrid:
		r.Stage = StageAgent
	case StageAgent:
		return nil, fmt.Errorf("%w: %q", ErrAlreadyAtAgent, ruleID)
	}
	d.To = r.Stage
	rs.rules[ruleID] = r
	return d, rs.commit(VerbDemote, ruleID, fmt.Sprintf("%s: %s", f, reason))
}

// Forget removes a rule.
//
// It costs exactly what installing cost: the same call shape, the same
// audit shape, and no extra evidence bar. That symmetry is deliberate
// and is the one place this design refuses to imitate its own
// inspiration — in motor learning unlearning is more expensive than
// learning, and a memory that inherited that asymmetry would accumulate
// rigidity by construction.
func (rs *RuleSet) Forget(ruleID, reason string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if _, ok := rs.rules[ruleID]; !ok {
		return fmt.Errorf("ladder: no rule %q", ruleID)
	}
	delete(rs.rules, ruleID)
	return rs.commit(VerbForget, ruleID, reason)
}

// Attachment is one stack's validated view of a rule set.
//
// It is a SNAPSHOT, not a subscription. A live-mutating rule set would
// change a running ladder's behaviour without re-passing the load-time
// gate — and that gate is where the authority check lives — so a stack
// picks up its siblings' learning at its next attach, not mid-flight.
type Attachment struct {
	Flavour     string
	Fingerprint string
	Rules       []Recalled
	scope       *Scope
}

// Scope returns the scope this attachment was validated against.
func (a *Attachment) Scope() *Scope { return a.scope }

// Attach validates the rule set against ONE stack's scope and returns
// that stack's snapshot.
//
// Validation is per attachment and not once per set. Two stacks of a
// flavour can hold different manifests — one may reap and spawn, another
// only reap — so a rule naming an action the attaching stack may not
// perform is refused HERE. A shared set validated once and trusted
// everywhere would be authority leaking between stacks.
//
// The check needs the consumer's help, because claudia does not
// interpret rule bodies: actionsOf reports which actions a rule names.
// Returning nothing means the rule names none.
func (rs *RuleSet) Attach(scope *Scope, actionsOf func(r *Recalled) ([]string, error)) (*Attachment, error) {
	if scope == nil {
		return nil, fmt.Errorf("ladder: attaching a rule set needs a scope")
	}
	rules := rs.Rules()

	if actionsOf != nil {
		for i := range rules {
			named, err := actionsOf(&rules[i])
			if err != nil {
				return nil, fmt.Errorf("ladder: reading actions of rule %q: %w", rules[i].RuleID, err)
			}
			for _, name := range named {
				if _, err := scope.LookupAction(name); err != nil {
					return nil, fmt.Errorf("ladder: rule %q cannot attach to layer %q: %w", rules[i].RuleID, scope.Layer(), err)
				}
			}
		}
	}

	fp, err := Fingerprint(rules)
	if err != nil {
		return nil, err
	}
	return &Attachment{Flavour: rs.Flavour, Fingerprint: fp, Rules: rules, scope: scope}, nil
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"fmt"
	"sync"
)

// RuleSet is procedural memory for one FLAVOUR of agent: what stacks of
// that kind have learned to do.
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
type RuleSet struct {
	// Flavour names the kind of agent these rules belong to. It appears
	// in journal records so pooled evidence stays attributable.
	Flavour string

	mu    sync.RWMutex
	rules []Recalled
}

// NewRuleSet builds a rule set for a flavour, seeded with rules from an
// earlier run.
//
// A seeded set is INDISTINGUISHABLE from one that learned the same
// rules: restoring takes no separate code path, so there is no second
// behaviour to keep in step.
func NewRuleSet(flavour string, seed []Recalled) (*RuleSet, error) {
	if flavour == "" {
		return nil, fmt.Errorf("ladder: a rule set needs a flavour — pooled evidence must stay attributable")
	}
	seen := make(map[string]bool, len(seed))
	for i, r := range seed {
		if r.RuleID == "" {
			return nil, fmt.Errorf("ladder: seed rule %d has no id", i)
		}
		if seen[r.RuleID] {
			return nil, fmt.Errorf("ladder: seed names rule %q twice", r.RuleID)
		}
		seen[r.RuleID] = true
	}
	rs := &RuleSet{Flavour: flavour, rules: append([]Recalled(nil), seed...)}
	return rs, nil
}

// LoadRuleSet parses a saved rule set.
//
// It refuses malformed input rather than starting empty, because an
// empty memory is indistinguishable from one that never learned
// anything, and a ladder in that state quietly reverts to waking a model
// for everything while reporting perfect health.
func LoadRuleSet(flavour string, data []byte) (*RuleSet, error) {
	rules, err := UnmarshalRules(data)
	if err != nil {
		return nil, err
	}
	return NewRuleSet(flavour, rules)
}

// Save renders the rule set as deterministic YAML for the consumer to
// store. Claudia never writes a file.
func (rs *RuleSet) Save() ([]byte, error) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return MarshalRules(rs.rules)
}

// Rules returns a copy of the current rule set.
func (rs *RuleSet) Rules() []Recalled {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return append([]Recalled(nil), rs.rules...)
}

// Fingerprint identifies the current contents.
func (rs *RuleSet) Fingerprint() (string, error) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return Fingerprint(rs.rules)
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

	rs.mu.RLock()
	rules := append([]Recalled(nil), rs.rules...)
	rs.mu.RUnlock()

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

// Apply replaces the rule set's contents, which is what a consolidation
// pass produces. Attached stacks are unaffected until they attach again.
func (rs *RuleSet) Apply(rules []Recalled) error {
	seen := make(map[string]bool, len(rules))
	for i, r := range rules {
		if r.RuleID == "" {
			return fmt.Errorf("ladder: rule %d has no id", i)
		}
		if seen[r.RuleID] {
			return fmt.Errorf("ladder: rule %q appears twice", r.RuleID)
		}
		seen[r.RuleID] = true
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.rules = append([]Recalled(nil), rules...)
	return nil
}

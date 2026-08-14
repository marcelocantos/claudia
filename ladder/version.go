// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"errors"
	"fmt"
	"slices"
)

// Verb is what produced a version. Every change to the rule set is one of
// these, so the history reads as a sequence of acts rather than a series
// of unexplained states.
type Verb string

const (
	// VerbSeed is the version a rule set starts at, whether empty or
	// restored from an earlier run.
	VerbSeed Verb = "seed"
	// VerbInstall is a proposal promoted into the set.
	VerbInstall Verb = "install"
	// VerbDemote is a rule moved down a stage.
	VerbDemote Verb = "demote"
	// VerbForget is a rule removed.
	VerbForget Verb = "forget"
	// VerbRollback is the set restored to the contents of an earlier
	// version. It is a move FORWARD in the history that happens to
	// reinstate earlier content — the record of what was rolled back is
	// itself worth keeping.
	VerbRollback Verb = "rollback"
)

// Version is one committed state of the rule set: the rules as they
// stood, and the act that got them there.
type Version struct {
	// Fingerprint identifies the CONTENT, so two versions holding the
	// same rules carry the same fingerprint however they arrived there.
	// That is what makes a rollback checkable: a replay pinned to the
	// restored version is comparable with the one that ran before.
	Fingerprint string

	// Verb is what produced this version.
	Verb Verb

	// RuleID is the rule the verb acted on. It is empty for a seed or a
	// rollback, which act on the set rather than on one rule.
	RuleID string

	// Reason is the consumer's note, carried so a demotion or a rollback
	// explains itself later without anyone reconstructing the pass it
	// happened in.
	Reason string

	rules []Recalled
}

// Rules returns the rule set as it stood at this version.
func (v *Version) Rules() []Recalled { return slices.Clone(v.rules) }

// ErrNoSuchVersion reports a rollback to a fingerprint the store has
// never held. It is typed because a consumer responds to it differently
// from a broken store: the version it wanted may simply have decayed out
// of a bounded history it kept elsewhere.
var ErrNoSuchVersion = errors.New("ladder: no such version")

// commit appends a version for the change just made. Caller holds the
// write lock.
func (rs *RuleSet) commit(verb Verb, ruleID, reason string) error {
	rules := rs.sorted()
	fp, err := Fingerprint(rules)
	if err != nil {
		return err
	}
	rs.history = append(rs.history, Version{
		Fingerprint: fp,
		Verb:        verb,
		RuleID:      ruleID,
		Reason:      reason,
		rules:       rules,
	})
	return nil
}

// Pin returns the current version.
//
// It is the verb a replay is built on: behaviour is history-dependent
// once rules accumulate, so a measurement that does not name the version
// it ran against is measuring a moving target. Pinning is free and
// non-exclusive — it takes a snapshot, it does not lock the set — because
// a pin that blocked learning would make the honest thing the expensive
// one.
func (rs *RuleSet) Pin() Version {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.history[len(rs.history)-1]
}

// Versions returns the history, oldest first.
//
// It is append-only. A store that edited its own past would be unable to
// answer the one question an audit asks — what was in force when this
// decision was made — and every rule in the set claims to have been
// promoted on evidence.
func (rs *RuleSet) Versions() []Version {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return slices.Clone(rs.history)
}

// Rollback restores the rules of an earlier version.
//
// The restored state fingerprints identically to the version it came
// from, because the fingerprint is over content: a replay pinned to the
// rolled-back version is directly comparable with the one that ran when
// those rules were first in force. The history still grows — the fact
// that a rollback happened, and why, is itself part of the record.
func (rs *RuleSet) Rollback(fingerprint, reason string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	var target *Version
	for i := range rs.history {
		if rs.history[i].Fingerprint == fingerprint {
			target = &rs.history[i]
		}
	}
	if target == nil {
		return fmt.Errorf("%w: %s", ErrNoSuchVersion, fingerprint)
	}

	rules := make(map[string]Recalled, len(target.rules))
	for _, r := range target.rules {
		rules[r.RuleID] = r
	}
	rs.rules = rules
	return rs.commit(VerbRollback, "", reason)
}

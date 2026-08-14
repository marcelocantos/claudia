// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"errors"
	"fmt"
	"sort"
)

// ErrNoSimilarity reports a recall with no similarity function. Claudia
// holds no embedding model and will not acquire one — scoring prose is
// exactly the judgement a runtime must not make on the consumer's
// behalf — and a default that fell back to substring matching would be
// a retrieval policy compiled in, silently, under a name that promised
// none.
var ErrNoSimilarity = errors.New("ladder: recall needs a similarity function — claudia does not score prose")

// RecallArgs configures retrieval of rules by what they are FOR.
type RecallArgs struct {
	// Query describes the situation at hand, in the same prose register
	// as the rule descriptions it is scored against.
	Query string

	// Similar scores a query against one rule's description, higher
	// being closer. An embedding cosine is the expected implementation;
	// anything ordinal works.
	//
	// It sees the two prose strings and NOTHING ELSE. That is the whole
	// point of the criterion this satisfies: a rule is retrieved by the
	// situation it handles, not by matching an exhaustive predicate, so
	// handing the scorer the rule body would reintroduce precisely the
	// keying this design rejects.
	Similar func(query, description string) float64

	// MinScore is the cutoff, below which a rule is not recalled. It is
	// the consumer's, because what counts as close enough depends on the
	// scorer and on what a wrong recall costs — neither of which claudia
	// knows.
	MinScore float64

	// Limit caps how many rules come back, best first. Zero means all
	// that clear MinScore.
	Limit int

	// Stages restricts recall to rules at these stages. Empty means any.
	// A consumer asking "what can I answer without a model?" wants
	// [StageDeterministic] alone.
	Stages []Stage
}

// Recalled rules with their scores, best first.
type Match struct {
	Rule  Recalled
	Score float64
}

// Recall returns the rules whose descriptions best fit the query.
//
// This is the retrieval half of carrying a prose description at mint
// time, and it is a seam rather than an implementation: the consumer
// supplies the scorer, claudia supplies the ordering, the cutoff
// plumbing and the stage filter. It is also what makes a deterministic
// action explainable without invoking a model — the description that
// found the rule is the description that justifies it.
//
// Ties break on rule ID, so two rules a scorer cannot separate come back
// in a stable order rather than in map order.
func (rs *RuleSet) Recall(args *RecallArgs) ([]Match, error) {
	switch {
	case args == nil:
		return nil, fmt.Errorf("ladder: nil RecallArgs")
	case args.Similar == nil:
		return nil, ErrNoSimilarity
	case args.Query == "":
		return nil, fmt.Errorf("ladder: recall needs a query — an empty one scores every rule against nothing")
	}

	var wanted map[Stage]bool
	if len(args.Stages) > 0 {
		wanted = make(map[Stage]bool, len(args.Stages))
		for _, s := range args.Stages {
			wanted[s] = true
		}
	}

	var matches []Match
	for _, r := range rs.Rules() {
		if wanted != nil && !wanted[r.Stage] {
			continue
		}
		score := args.Similar(args.Query, r.Description)
		if score < args.MinScore {
			continue
		}
		matches = append(matches, Match{Rule: r, Score: score})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].Rule.RuleID < matches[j].Rule.RuleID
	})
	if args.Limit > 0 && len(matches) > args.Limit {
		matches = matches[:args.Limit]
	}
	return matches, nil
}

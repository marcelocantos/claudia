// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// described is a rule carrying the prose that recall keys on.
func described(id, description string, stage ladder.Stage) ladder.Recalled {
	return ladder.Recalled{
		RuleID: id, Class: "worker",
		Description: description,
		Stage:       stage,
		Evidence:    goodEvidence(),
	}
}

// wordOverlap is a consumer's scorer standing in for an embedding
// cosine: the share of query words the description also uses. It is
// deliberately crude, because what the test asserts is that claudia
// ranks and filters by whatever the consumer's numbers say, not that
// any particular notion of similarity is right.
func wordOverlap(query, description string) float64 {
	have := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(description)) {
		have[w] = true
	}
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return 0
	}
	var hits int
	for _, w := range words {
		if have[w] {
			hits++
		}
	}
	return float64(hits) / float64(len(words))
}

func recallFixture(t *testing.T) *ladder.RuleSet {
	t.Helper()
	return mustRuleSet(t, []ladder.Recalled{
		described("reap", "retire a worker that has finished and reported", ladder.StageDeterministic),
		described("unblock", "wake a worker that is blocked waiting on review", ladder.StageHybrid),
		described("spawn", "start a worker for a newly filed target", ladder.StageDeterministic),
	})
}

// A rule is found by the situation it handles, not by an exhaustive
// predicate — which is the whole reason a description is requested at
// mint time.
func TestRulesAreRecalledByWhatTheyAreFor(t *testing.T) {
	rs := recallFixture(t)

	got, err := rs.Recall(&ladder.RecallArgs{
		Query: "worker finished", Similar: wordOverlap, MinScore: 0.75,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every rule here is about workers, so a lax cutoff would return all
	// three. The cutoff is the consumer's, and at 0.75 only the rule
	// actually about a finished worker clears it.
	if len(got) != 1 || got[0].Rule.RuleID != "reap" {
		t.Fatalf("recall = %v, want just reap", ids(got))
	}
	if got[0].Score != 1 {
		t.Errorf("score = %v, want 1 — both query words are in the description", got[0].Score)
	}

	// Nothing in the set is about deploys, and recall says so rather
	// than returning its least-bad rule.
	none, err := rs.Recall(&ladder.RecallArgs{
		Query: "roll back a deploy", Similar: wordOverlap, MinScore: 0.75,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("recall = %v, want nothing: no rule covers this", ids(none))
	}
}

func TestRecallRanksBestFirstAndBreaksTiesStably(t *testing.T) {
	// "bravo" sorts between the other two by ID and scores below both,
	// so ID order and score order disagree — which is what makes this
	// test say anything. Rules come back from the set in ID order
	// already, so a recall that forgot to rank would pass otherwise.
	rs := mustRuleSet(t, []ladder.Recalled{
		described("zeta", "retire a worker", ladder.StageDeterministic),
		described("alpha", "retire a worker", ladder.StageDeterministic),
		described("bravo", "worker maintenance of every kind", ladder.StageDeterministic),
	})

	got, err := rs.Recall(&ladder.RecallArgs{
		Query: "retire worker", Similar: wordOverlap,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "zeta", "bravo"}; !slices.Equal(ids(got), want) {
		t.Errorf("recall = %v, want %v — best first, ties on rule ID", ids(got), want)
	}

	limited, err := rs.Recall(&ladder.RecallArgs{
		Query: "retire worker", Similar: wordOverlap, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "zeta"}; !slices.Equal(ids(limited), want) {
		t.Errorf("limited recall = %v, want %v — a limit keeps the best, not the first", ids(limited), want)
	}
}

// "What can I answer without waking a model?" is a stage question, and
// it is the question a ladder asks on the hot path.
func TestRecallFiltersByStage(t *testing.T) {
	rs := recallFixture(t)

	got, err := rs.Recall(&ladder.RecallArgs{
		Query: "worker", Similar: wordOverlap,
		Stages: []ladder.Stage{ladder.StageDeterministic},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"reap", "spawn"}; !slices.Equal(ids(got), want) {
		t.Errorf("recall = %v, want %v — the hybrid rule is not answerable without a model", ids(got), want)
	}
}

// Claudia holds no embedding model, and a default scorer would be a
// retrieval policy compiled into a runtime that has none. It refuses
// instead.
func TestRecallRefusesToScoreProseItself(t *testing.T) {
	rs := recallFixture(t)

	if _, err := rs.Recall(&ladder.RecallArgs{Query: "worker finished"}); !errors.Is(err, ladder.ErrNoSimilarity) {
		t.Errorf("recall with no scorer: err = %v, want ErrNoSimilarity", err)
	}
	if _, err := rs.Recall(&ladder.RecallArgs{Similar: wordOverlap}); err == nil {
		t.Error("an empty query was accepted; it scores every rule against nothing")
	}
}

// The scorer sees the query and the description and nothing else, so a
// consumer cannot quietly go back to keying on the rule body.
func TestRecallShowsTheScorerNothingButProse(t *testing.T) {
	rs := recallFixture(t)

	seen := make(map[string]bool)
	if _, err := rs.Recall(&ladder.RecallArgs{
		Query: "worker finished",
		Similar: func(query, description string) float64 {
			seen[description] = true
			return wordOverlap(query, description)
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range rs.Rules() {
		if !seen[r.Description] {
			t.Errorf("rule %q was scored on something other than its description", r.RuleID)
		}
	}
}

func ids(matches []ladder.Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Rule.RuleID)
	}
	return out
}

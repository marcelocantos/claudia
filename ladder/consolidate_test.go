// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// workPhase deposits episodes as N stacks of one flavour do their work,
// all answered by a model because nothing has been learned yet.
func workPhase(t *testing.T, j ladder.Journal, fingerprint string, stacks []string, perStack int, delivered func(stack string, i int) bool) {
	t.Helper()
	for _, stack := range stacks {
		for i := range perStack {
			id := fmt.Sprintf("%s-%d", stack, i)
			if err := j.Record(&ladder.Episode{
				ID: id, Stack: stack, Fingerprint: fingerprint,
				Kind: "worker.finished", AnsweredBy: "model", Escalated: true,
			}); err != nil {
				t.Fatal(err)
			}
			if err := j.Judge(id, &ladder.Outcome{Delivered: delivered(stack, i)}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func newPass(t *testing.T, f *fixture, j ladder.Journal, rs *ladder.RuleSet) *ladder.ConsolidationPass {
	t.Helper()
	return &ladder.ConsolidationPass{
		Rules:   rs,
		Journal: j,
		Scope:   f.scope(t, "pattern", nil, []ladder.Action{f.reap}),
	}
}

func findCandidate(cands []ladder.Candidate, kind, class string) *ladder.Candidate {
	for i := range cands {
		if cands[i].Kind == kind && (class == "" || cands[i].Class == class) {
			return &cands[i]
		}
	}
	return nil
}

func TestConsolidateProposesAndInstallsNothing(t *testing.T) {
	f := newFixture()
	rs, err := ladder.NewRuleSet("po-flavour", nil)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := rs.Fingerprint()

	j := ladder.NewMemoryJournal(0)
	workPhase(t, j, fp, []string{"po-a", "po-b"}, 30, func(string, int) bool { return true })

	cands, err := ladder.Consolidate(context.Background(), newPass(t, f, j, rs))
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	c := findCandidate(cands, ladder.CandidatePromote, "worker.finished")
	if c == nil {
		t.Fatalf("nothing proposed from 60 clean episodes: %+v", cands)
	}
	if !c.HoldsEverywhere || c.Blocked != "" {
		t.Errorf("a candidate holding on both stacks was blocked: %+v", c)
	}
	if len(c.PerStack) != 2 {
		t.Errorf("per-stack breakdown = %+v, want both stacks", c.PerStack)
	}

	// The rule set is untouched. The pass that reasons is not the pass
	// that acts, and that is a fact about the signature.
	if len(rs.Rules()) != 0 {
		t.Errorf("Consolidate installed %d rules", len(rs.Rules()))
	}
	if after, _ := rs.Fingerprint(); after != fp {
		t.Error("Consolidate changed the rule set")
	}
}

// The hazard that arrives with sharing a rule set across a flavour: a
// candidate can look excellent on the pool and be wrong for a member.
func TestPooledEvidenceMustHoldPerStack(t *testing.T) {
	f := newFixture()
	rs, _ := ladder.NewRuleSet("po-flavour", nil)
	fp, _ := rs.Fingerprint()

	j := ladder.NewMemoryJournal(0)
	// One stack is a disaster; the other is so much larger that the
	// pooled figure still clears the gate.
	workPhase(t, j, fp, []string{"po-good"}, 90, func(string, int) bool { return true })
	workPhase(t, j, fp, []string{"po-bad"}, 10, func(string, int) bool { return false })

	cands, err := ladder.Consolidate(context.Background(), newPass(t, f, j, rs))
	if err != nil {
		t.Fatal(err)
	}
	c := findCandidate(cands, ladder.CandidatePromote, "worker.finished")
	if c == nil {
		t.Fatalf("no candidate raised: %+v", cands)
	}

	if c.Evidence.Consistency() < 0.9 {
		t.Fatalf("the pooled figure should look fine: %.2f", c.Evidence.Consistency())
	}
	if c.HoldsEverywhere {
		t.Error("a candidate failing on one stack was reported as holding everywhere")
	}
	if !strings.Contains(c.Blocked, "homogeneous") {
		t.Errorf("the blocking reason does not name the pooling hazard: %q", c.Blocked)
	}
}

func TestCandidateExceedingScopeIsBlockedNotProposed(t *testing.T) {
	f := newFixture()
	rs, _ := ladder.NewRuleSet("po-flavour", nil)
	fp, _ := rs.Fingerprint()

	j := ladder.NewMemoryJournal(0)
	workPhase(t, j, fp, []string{"po-a"}, 60, func(string, int) bool { return true })

	pass := newPass(t, f, j, rs)
	// The candidate would name an action this layer may not perform.
	pass.ActionsOf = func(string) []string { return []string{"agent.spawn"} }

	cands, err := ladder.Consolidate(context.Background(), pass)
	if err != nil {
		t.Fatal(err)
	}
	c := findCandidate(cands, ladder.CandidatePromote, "worker.finished")
	if c == nil {
		t.Fatal("no candidate raised")
	}
	if c.Blocked == "" || !strings.Contains(c.Blocked, "agent.spawn") {
		t.Errorf("a candidate exceeding its scope was proposed unblocked: %+v", c)
	}
}

func TestConsolidationProposesRemovalsNotOnlyAdditions(t *testing.T) {
	f := newFixture()
	rs, _ := ladder.NewRuleSet("po-flavour", []ladder.Recalled{learned("stale", "worker.vanished", goodEvidence())})
	j := ladder.NewMemoryJournal(0)

	pass := newPass(t, f, j, rs)
	pass.Symptoms = []ladder.Symptom{
		{Kind: ladder.SymptomInert, RuleID: "stale", Class: "worker.vanished", Detail: "removing it changes no outcome"},
		{Kind: ladder.SymptomContradicted, RuleID: "wrong", Class: "worker.idle", Detail: "denied 4 times"},
	}

	cands, err := ladder.Consolidate(context.Background(), pass)
	if err != nil {
		t.Fatal(err)
	}

	// A pass that only ever adds makes the store monotonically more
	// rigid, which is the failure the motor-learning frame predicts.
	if c := findCandidate(cands, ladder.CandidateRevoke, "worker.vanished"); c == nil {
		t.Error("an inert rule produced no revocation candidate")
	}
	if c := findCandidate(cands, ladder.CandidateDemote, "worker.idle"); c == nil {
		t.Error("a contradicted rule produced no demotion candidate")
	}
}

func TestEpisodesFromAnotherRuleSetAndUnjudgedOnesAreNotEvidence(t *testing.T) {
	f := newFixture()
	rs, _ := ladder.NewRuleSet("po-flavour", nil)
	fp, _ := rs.Fingerprint()
	j := ladder.NewMemoryJournal(0)

	// Plenty of episodes, but from a world that no longer exists.
	workPhase(t, j, "sha256:someotherworld", []string{"po-a"}, 80, func(string, int) bool { return true })

	// And plenty that were never judged. No feedback is NO evidence —
	// not weak assent.
	for i := range 80 {
		if err := j.Record(&ladder.Episode{
			ID: fmt.Sprintf("unjudged-%d", i), Stack: "po-a", Fingerprint: fp,
			Kind: "worker.finished", AnsweredBy: "model",
		}); err != nil {
			t.Fatal(err)
		}
	}

	cands, err := ladder.Consolidate(context.Background(), newPass(t, f, j, rs))
	if err != nil {
		t.Fatal(err)
	}
	if c := findCandidate(cands, ladder.CandidatePromote, ""); c != nil {
		t.Errorf("promoted on episodes that were unjudged or from another rule set: %+v", c)
	}
}

func TestConsolidateRefusesWithoutWhatItCannotInfer(t *testing.T) {
	f := newFixture()
	rs, _ := ladder.NewRuleSet("f", nil)
	j := ladder.NewMemoryJournal(0)
	ctx := context.Background()

	for name, br := range map[string]func(*ladder.ConsolidationPass){
		"no rules":   func(p *ladder.ConsolidationPass) { p.Rules = nil },
		"no journal": func(p *ladder.ConsolidationPass) { p.Journal = nil },
		"no scope":   func(p *ladder.ConsolidationPass) { p.Scope = nil },
	} {
		t.Run(name, func(t *testing.T) {
			pass := newPass(t, f, j, rs)
			br(pass)
			if _, err := ladder.Consolidate(ctx, pass); err == nil {
				t.Error("consolidation ran without it")
			}
		})
	}
	if _, err := ladder.Consolidate(ctx, nil); err == nil {
		t.Error("consolidation ran on nil")
	}
}

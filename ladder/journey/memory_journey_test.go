// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package journey

import (
	"context"
	"fmt"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// workUntil runs the corpus until a class has been seen `want` times,
// depositing an episode for every request and judging each one.
//
// This is the working half of the cycle: the stack decides, and what
// happened is deposited as it happens rather than recalled afterwards.
func workUntil(t *testing.T, e *Env, j ladder.Journal, l *ladder.Ladder, fingerprint, stack, class string, want int) int {
	t.Helper()
	seen, pass := 0, 0
	for seen < want {
		if pass++; pass > 100 {
			t.Fatalf("%q never accumulated: %d after %d passes", class, seen, pass)
		}
		for i, req := range e.Corpus() {
			walked := &ladder.Request{Kind: req.Kind, Payload: req.Payload}
			res, err := l.Evaluate(context.Background(), walked)
			if err != nil {
				continue
			}
			id := fmt.Sprintf("%s-%d-%d", stack, pass, i)
			if err := j.Record(ladder.EpisodeFrom(id, stack, fingerprint, walked, res)); err != nil {
				t.Fatal(err)
			}
			// The label arrives after the fact, as a second write.
			if err := j.Judge(id, &ladder.Outcome{Delivered: Delivered(walked, res)}); err != nil {
				t.Fatal(err)
			}
			if walked.Kind == class {
				seen++
			}
		}
	}
	return seen
}

// Journey 9: the whole memory cycle, across a simulated restart.
//
// Work deposits episodes; a consolidation pass reads them and proposes;
// the proposals are installed; the rule set is SAVED, THROWN AWAY AND
// RELOADED — which is the "across instances of the stack" requirement
// made literal — and the second working phase is measurably cheaper
// without losing delivered work.
//
// This is the test whose absence let two incompatible rule containers
// survive review: it cannot be written unless every piece of the loop
// actually joins to the next one.
func TestJourneyMemoryCycleSurvivesARestart(t *testing.T) {
	e := NewEnv(t)
	journal := ladder.NewMemoryJournal(0)

	fp0, err := e.Rules.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	// PHASE ONE — work. Nothing is learned yet, so a model answers
	// everything and the episodes pile up.
	modelOnly := ladder.New(e.ModelRung())
	before := e.ReplayAt(e.Rules.Rules())
	if before.Stats.ModelShare() != 1 {
		t.Fatalf("baseline model share = %.2f, want 1", before.Stats.ModelShare())
	}
	workUntil(t, e, journal, modelOnly, fp0, "po-a", "worker.finished", 60)
	if len(journal.Episodes()) == 0 {
		t.Fatal("work deposited no episodes")
	}

	// SLEEP — the consolidation pass, scheduled here rather than
	// triggered by the work it generalises.
	scope := e.Scope("pattern", nil, []ladder.Action{e.Reap})
	cands, err := ladder.Consolidate(context.Background(), &ladder.ConsolidationPass{
		Rules:       e.Rules,
		Journal:     journal,
		Scope:       scope,
		ModelLayers: []string{"model"},
	})
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	var promote *ladder.Candidate
	for i := range cands {
		if cands[i].Kind == ladder.CandidatePromote && cands[i].Blocked == "" {
			promote = &cands[i]
		}
	}
	if promote == nil {
		t.Fatalf("consolidation proposed nothing installable: %+v", cands)
	}
	if !promote.HoldsEverywhere {
		t.Errorf("a single-stack candidate did not hold everywhere: %+v", promote)
	}

	// The pass proposed and installed nothing itself.
	if fpAfterPass, _ := e.Rules.Fingerprint(); fpAfterPass != fp0 {
		t.Error("Consolidate changed the rule set; it must only propose")
	}

	// INSTALL — a separate act, in a later pass, by someone other than
	// the proposer. Both gates are live here, not stubbed.
	rule := narrowRule("worker.finished")
	if err := e.Rules.Propose(&ladder.Proposal{
		ID: rule.ID, Class: promote.Class, Description: promote.Why,
		Rule: rule, ProposedBy: "consolidation-reasoning", Pass: "sleep",
		Evidence: promote.Evidence,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Rules.Install(&ladder.InstallArgs{
		ProposalID: rule.ID, Installer: "operator", Pass: "install", Stage: ladder.StageDeterministic,
	}); err != nil {
		t.Fatalf("install refused: %v", err)
	}

	// RESTART — save, discard, reload. This is the whole point: what was
	// learned must outlive the process that learned it.
	saved, err := e.Rules.Save()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ladder.LoadRuleSet("journey", saved, nil)
	if err != nil {
		t.Fatalf("a saved rule set would not reload: %v", err)
	}
	fpBefore, _ := e.Rules.Fingerprint()
	fpAfter, _ := reloaded.Fingerprint()
	if fpBefore != fpAfter {
		t.Errorf("the restart changed the rules: %s vs %s", fpBefore, fpAfter)
	}

	// ATTACH — the fresh instance revalidates against its own scope, and
	// decoding the opaque body is the consumer's side of the boundary.
	att, err := reloaded.Attach(scope, func(r *ladder.Recalled) ([]string, error) {
		var def ladder.RuleDef
		if err := r.Decode(&def); err != nil {
			return nil, err
		}
		if def.Action == "" {
			return nil, nil
		}
		return []string{def.Action}, nil
	})
	if err != nil {
		t.Fatalf("the reloaded rule set would not attach: %v", err)
	}
	if len(att.Rules) != 1 || att.Rules[0].RuleID != rule.ID {
		t.Fatalf("attachment holds %+v", att.Rules)
	}

	// PHASE TWO — the same work, now against what was learned.
	after := e.ReplayAt(att.Rules)
	cmp, err := ladder.CompareReplays(before, after)
	if err != nil {
		t.Fatal(err)
	}

	if !cmp.Cheaper {
		t.Errorf("a full cycle produced no saving: %s", cmp.Summary())
	}
	if cmp.OversightRegression {
		t.Errorf("the cycle bought its saving with delivered work: %s", cmp.Summary())
	}
	if cmp.DeliveredShareDelta != 0 {
		t.Errorf("delivered work moved across the cycle: %s", cmp.Summary())
	}
	if after.Stats.AnsweredBy["pattern"] == 0 {
		t.Error("the learned rule never fired after the restart")
	}
	if after.Stats.AnsweredBy["model"] == 0 {
		t.Error("nothing reaches the model any more; the rule learned too much")
	}
}

// The consumer side of the codec boundary: claudia carries a body it
// does not understand, and only the consumer can read it back.
func TestOpaqueRuleBodiesSurviveARoundTrip(t *testing.T) {
	e := NewEnv(t)
	original := narrowRule("worker.finished")

	if err := e.Rules.Propose(&ladder.Proposal{
		ID: original.ID, Class: "worker.finished", Description: "d",
		Rule: original, ProposedBy: "work", Pass: "p1",
		Evidence: ladder.Evidence{Runs: 60, Identical: 60, TestsPass: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Rules.Install(&ladder.InstallArgs{
		ProposalID: original.ID, Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic,
	}); err != nil {
		t.Fatal(err)
	}

	saved, err := e.Rules.Save()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ladder.LoadRuleSet("journey", saved, nil)
	if err != nil {
		t.Fatal(err)
	}

	rules := reloaded.Rules()
	var got ladder.RuleDef
	if err := rules[0].Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got.ID != original.ID || got.Action != original.Action || got.Description != original.Description {
		t.Errorf("body did not survive: %+v vs %+v", got, original)
	}
	if len(got.When) != 1 || got.When[0].Field != "kind" || got.When[0].Kind != ladder.Equals || got.When[0].Value != "worker.finished" {
		t.Errorf("predicates did not survive: %+v", got.When)
	}

	// And the decoded rule still compiles into a working layer, which is
	// the only proof that survives contact with reality.
	if _, err := ladder.NewPatternLayer(&ladder.PatternConfig{
		Scope:  e.Scope("pattern", nil, []ladder.Action{e.Reap}),
		Fields: Fields(),
		Rules:  []ladder.RuleDef{got},
	}); err != nil {
		t.Errorf("a round-tripped rule would not load: %v", err)
	}
}

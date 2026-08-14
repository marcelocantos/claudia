// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"errors"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// mustRuleSet builds a rule set or fails the test.
func mustRuleSet(t *testing.T, seed []ladder.Recalled) *ladder.RuleSet {
	t.Helper()
	rs, err := ladder.NewRuleSet(&ladder.RuleSetConfig{Flavour: "test", Seed: seed})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	return rs
}

func goodEvidence() ladder.Evidence {
	return ladder.Evidence{Runs: 60, Identical: 60, TestsPass: true, Corrections: 5}
}

func propose(t *testing.T, s *ladder.RuleSet, id, by, pass string, ev ladder.Evidence) {
	t.Helper()
	if err := s.Propose(&ladder.Proposal{
		ID:          id,
		Class:       "worker.finished",
		Description: "retire a finished worker",
		ProposedBy:  by,
		Pass:        pass,
		Evidence:    ev,
	}); err != nil {
		t.Fatalf("Propose: %v", err)
	}
}

func TestProposerMayNotInstallItsOwnRule(t *testing.T) {
	s := mustRuleSet(t, nil)
	propose(t, s, "r1", "pattern-layer", "pass-1", goodEvidence())

	err := s.Install(&ladder.InstallArgs{
		ProposalID: "r1", Installer: "pattern-layer", Pass: "pass-2", Stage: ladder.StageDeterministic,
	})
	if !errors.Is(err, ladder.ErrSelfInstall) {
		t.Fatalf("err = %v, want ErrSelfInstall — the party whose cost a rule reduces must not install it", err)
	}
	if len(s.Rules()) != 0 {
		t.Error("a refused install still landed a rule")
	}

	// Someone else, in a later pass, may.
	if err := s.Install(&ladder.InstallArgs{
		ProposalID: "r1", Installer: "consolidator", Pass: "pass-2", Stage: ladder.StageDeterministic,
	}); err != nil {
		t.Fatalf("legitimate install refused: %v", err)
	}
	if _, ok := s.Lookup("r1"); !ok {
		t.Error("installed rule is not in the rule set")
	}
}

func TestInstallCannotHappenInThePassThatProposed(t *testing.T) {
	s := mustRuleSet(t, nil)
	propose(t, s, "r1", "worker", "pass-1", goodEvidence())

	err := s.Install(&ladder.InstallArgs{
		ProposalID: "r1", Installer: "consolidator", Pass: "pass-1", Stage: ladder.StageDeterministic,
	})
	if !errors.Is(err, ladder.ErrSamePass) {
		t.Fatalf("err = %v, want ErrSamePass — the separation is temporal", err)
	}

	// The same agent identity may install its own proposal in a LATER
	// pass. Propose while working, install while consolidating: the
	// constraint is on when, not on who it must be.
	if err := s.Install(&ladder.InstallArgs{
		ProposalID: "r1", Installer: "consolidator", Pass: "pass-2", Stage: ladder.StageDeterministic,
	}); err != nil {
		t.Fatalf("install in a later pass refused: %v", err)
	}
}

func TestPromotionGatesOnEvidence(t *testing.T) {
	tests := map[string]struct {
		ev    ladder.Evidence
		stage ladder.Stage
		ok    bool
	}{
		"deterministic with enough runs":  {ladder.Evidence{Runs: 50, Identical: 50, TestsPass: true}, ladder.StageDeterministic, true},
		"deterministic with too few runs": {ladder.Evidence{Runs: 49, Identical: 49, TestsPass: true}, ladder.StageDeterministic, false},
		"deterministic below consistency": {ladder.Evidence{Runs: 100, Identical: 98, TestsPass: true}, ladder.StageDeterministic, false},
		"hybrid with enough runs":         {ladder.Evidence{Runs: 10, Identical: 9, TestsPass: true}, ladder.StageHybrid, true},
		"hybrid below consistency":        {ladder.Evidence{Runs: 10, Identical: 8, TestsPass: true}, ladder.StageHybrid, false},
		"any safety violation":            {ladder.Evidence{Runs: 100, Identical: 100, TestsPass: true, SafetyViolations: 1}, ladder.StageHybrid, false},
		"failing acceptance tests":        {ladder.Evidence{Runs: 100, Identical: 100}, ladder.StageHybrid, false},
		"a correction says it was wrong":  {ladder.Evidence{Runs: 100, Identical: 100, TestsPass: true, Corrections: 3, CorrectionsAgainst: 1}, ladder.StageHybrid, false},
		"no runs is not perfect":          {ladder.Evidence{TestsPass: true}, ladder.StageHybrid, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := mustRuleSet(t, nil)
			propose(t, s, "r", "proposer", "p1", tc.ev)
			err := s.Install(&ladder.InstallArgs{ProposalID: "r", Installer: "other", Pass: "p2", Stage: tc.stage})
			if tc.ok && err != nil {
				t.Errorf("install refused: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Error("install accepted below threshold")
				} else if !errors.Is(err, ladder.ErrInsufficientEvidence) {
					t.Errorf("err = %v, want ErrInsufficientEvidence", err)
				}
			}
		})
	}
}

func TestConsistencyWithoutCorrectnessIsRecordedNotHidden(t *testing.T) {
	s := mustRuleSet(t, nil)

	// Repetition alone. Consistency proves a rule stable, never right —
	// a systematically wrong resolution promotes just as smoothly.
	propose(t, s, "stable", "proposer", "p1", ladder.Evidence{Runs: 60, Identical: 60, TestsPass: true})
	if err := s.Install(&ladder.InstallArgs{ProposalID: "stable", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic}); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Lookup("stable")
	if !e.ConsistencyOnly {
		t.Error("a rule promoted on repetition alone was not marked as such")
	}

	propose(t, s, "judged", "proposer", "p1", goodEvidence())
	if err := s.Install(&ladder.InstallArgs{ProposalID: "judged", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic}); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Lookup("judged")
	if e.ConsistencyOnly {
		t.Error("a rule with correctness signal was marked consistency-only")
	}
}

func TestDemotionNeedsNoEvidenceAndForgettingCostsWhatLearningCost(t *testing.T) {
	s := mustRuleSet(t, nil)
	propose(t, s, "r", "proposer", "p1", goodEvidence())
	if err := s.Install(&ladder.InstallArgs{ProposalID: "r", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic}); err != nil {
		t.Fatal(err)
	}

	// Stopping is always allowed: a circuit breaker that needed
	// evidence would not be one.
	if err := s.Demote("r", "output format changed upstream"); err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if e, _ := s.Lookup("r"); e.Stage != ladder.StageHybrid {
		t.Errorf("stage = %s, want hybrid", e.Stage)
	}
	if err := s.Demote("r", "still wrong"); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Lookup("r"); e.Stage != ladder.StageAgent {
		t.Errorf("stage = %s, want agent", e.Stage)
	}
	if err := s.Demote("r", "no further to fall"); err == nil {
		t.Error("demoting past agent was accepted")
	}

	// Forgetting takes the same shape as installing — same call, same
	// bar, which is none. Rigidity is what an asymmetry here would
	// accumulate.
	if err := s.Forget("r", "inert against the corpus"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, ok := s.Lookup("r"); ok {
		t.Error("a forgotten rule survives")
	}
	if err := s.Forget("r", "again"); err == nil {
		t.Error("forgetting an absent rule was accepted")
	}
}

// A rule set is identified by its content, so the same rules always
// fingerprint the same way and any change moves it.
func TestFingerprintMovesOnlyWhenTheRulesDo(t *testing.T) {
	s := mustRuleSet(t, nil)
	empty, err := s.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	propose(t, s, "r", "proposer", "p1", goodEvidence())
	if unchanged, _ := s.Fingerprint(); unchanged != empty {
		t.Error("an uninstalled proposal moved the fingerprint")
	}

	if err := s.Install(&ladder.InstallArgs{ProposalID: "r", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic}); err != nil {
		t.Fatal(err)
	}
	installed, _ := s.Fingerprint()
	if installed == empty {
		t.Error("installing a rule did not move the fingerprint")
	}

	if err := s.Forget("r", "undo"); err != nil {
		t.Fatal(err)
	}
	if back, _ := s.Fingerprint(); back != empty {
		t.Errorf("returning to the same rules gave a different fingerprint: %s vs %s", back, empty)
	}
}

func TestProposalsMustCarryWhatMakesThemAuditable(t *testing.T) {
	s := mustRuleSet(t, nil)
	base := ladder.Proposal{ID: "r", Description: "d", ProposedBy: "x", Pass: "p", Evidence: goodEvidence()}

	missing := map[string]func(p *ladder.Proposal){
		"id":          func(p *ladder.Proposal) { p.ID = "" },
		"proposer":    func(p *ladder.Proposal) { p.ProposedBy = "" },
		"pass":        func(p *ladder.Proposal) { p.Pass = "" },
		"description": func(p *ladder.Proposal) { p.Description = "" },
	}
	for name, strip := range missing {
		t.Run("without "+name, func(t *testing.T) {
			p := base
			strip(&p)
			if err := s.Propose(&p); err == nil {
				t.Error("proposal accepted without it")
			}
		})
	}

	if err := s.Propose(&base); err != nil {
		t.Fatal(err)
	}
	if err := s.Propose(&base); err == nil {
		t.Error("the same proposal was raised twice")
	}
	if got := s.Proposals(); len(got) != 1 || got[0] != "r" {
		t.Errorf("Proposals() = %v", got)
	}
}

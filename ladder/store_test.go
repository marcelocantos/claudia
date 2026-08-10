// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"errors"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// mustStore builds a store or fails the test.
func mustStore(t *testing.T, cfg *ladder.StoreConfig) *ladder.Store {
	t.Helper()
	s, err := ladder.NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func goodEvidence() ladder.Evidence {
	return ladder.Evidence{Runs: 60, Identical: 60, TestsPass: true, Corrections: 5}
}

func propose(t *testing.T, s *ladder.Store, id, by, pass string, ev ladder.Evidence) {
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
	s := mustStore(t, nil)
	propose(t, s, "r1", "pattern-layer", "pass-1", goodEvidence())

	_, err := s.Install(&ladder.InstallArgs{
		ProposalID: "r1", Installer: "pattern-layer", Pass: "pass-2", Stage: ladder.StageDeterministic,
	})
	if !errors.Is(err, ladder.ErrSelfInstall) {
		t.Fatalf("err = %v, want ErrSelfInstall — the party whose cost a rule reduces must not install it", err)
	}
	if s.Current().N != 0 {
		t.Error("a refused install still bumped the version")
	}

	// Someone else, in a later pass, may.
	if _, err := s.Install(&ladder.InstallArgs{
		ProposalID: "r1", Installer: "consolidator", Pass: "pass-2", Stage: ladder.StageDeterministic,
	}); err != nil {
		t.Fatalf("legitimate install refused: %v", err)
	}
	if _, ok := s.Current().Lookup("r1"); !ok {
		t.Error("installed rule is not in the current version")
	}
}

func TestInstallCannotHappenInThePassThatProposed(t *testing.T) {
	s := mustStore(t, nil)
	propose(t, s, "r1", "worker", "pass-1", goodEvidence())

	_, err := s.Install(&ladder.InstallArgs{
		ProposalID: "r1", Installer: "consolidator", Pass: "pass-1", Stage: ladder.StageDeterministic,
	})
	if !errors.Is(err, ladder.ErrSamePass) {
		t.Fatalf("err = %v, want ErrSamePass — the separation is temporal", err)
	}

	// The same agent identity may install its own proposal in a LATER
	// pass. Propose while working, install while consolidating: the
	// constraint is on when, not on who it must be.
	if _, err := s.Install(&ladder.InstallArgs{
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
			s := mustStore(t, nil)
			propose(t, s, "r", "proposer", "p1", tc.ev)
			_, err := s.Install(&ladder.InstallArgs{ProposalID: "r", Installer: "other", Pass: "p2", Stage: tc.stage})
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
	s := mustStore(t, nil)

	// Repetition alone. Consistency proves a rule stable, never right —
	// a systematically wrong resolution promotes just as smoothly.
	propose(t, s, "stable", "proposer", "p1", ladder.Evidence{Runs: 60, Identical: 60, TestsPass: true})
	if _, err := s.Install(&ladder.InstallArgs{ProposalID: "stable", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic}); err != nil {
		t.Fatal(err)
	}
	e, _ := s.Current().Lookup("stable")
	if !e.ConsistencyOnly {
		t.Error("a rule promoted on repetition alone was not marked as such")
	}

	propose(t, s, "judged", "proposer", "p1", goodEvidence())
	if _, err := s.Install(&ladder.InstallArgs{ProposalID: "judged", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic}); err != nil {
		t.Fatal(err)
	}
	e, _ = s.Current().Lookup("judged")
	if e.ConsistencyOnly {
		t.Error("a rule with correctness signal was marked consistency-only")
	}
}

func TestDemotionNeedsNoEvidenceAndRevocationCostsWhatPromotionCost(t *testing.T) {
	s := mustStore(t, nil)
	propose(t, s, "r", "proposer", "p1", goodEvidence())
	installed, err := s.Install(&ladder.InstallArgs{ProposalID: "r", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic})
	if err != nil {
		t.Fatal(err)
	}

	// Stopping is always allowed: a circuit breaker that needed
	// evidence would not be one.
	v, err := s.Demote("r", "output format changed upstream")
	if err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if e, _ := v.Lookup("r"); e.Stage != ladder.StageHybrid {
		t.Errorf("stage = %s, want hybrid", e.Stage)
	}
	if v, err = s.Demote("r", "still wrong"); err != nil {
		t.Fatal(err)
	}
	if e, _ := v.Lookup("r"); e.Stage != ladder.StageAgent {
		t.Errorf("stage = %s, want agent", e.Stage)
	}
	if _, err := s.Demote("r", "no further to fall"); err == nil {
		t.Error("demoting past agent was accepted")
	}

	// Revocation takes the same shape as installation — same version
	// bump, same audit record, no extra bar. Rigidity is what an
	// asymmetry here would accumulate.
	revoked, err := s.Revoke("r", "inert against the corpus")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := revoked.Lookup("r"); ok {
		t.Error("revoked rule survives in the current version")
	}
	if revoked.N <= installed.N {
		t.Error("revocation did not bump the version")
	}
}

func TestVersionsArePinnedImmutableAndRollbackIsItselfAnEvent(t *testing.T) {
	s := mustStore(t, nil)
	propose(t, s, "r", "proposer", "p1", goodEvidence())
	installed, err := s.Install(&ladder.InstallArgs{ProposalID: "r", Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic})
	if err != nil {
		t.Fatal(err)
	}

	// Pinning must address the version asked for, not whatever is
	// current when the call is made. Checked here, while the two
	// genuinely differ: version 0 predates the install, and the current
	// version holds the rule.
	empty, err := s.Pin(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Entries) != 0 {
		t.Errorf("version 0 has %d entries; Pin is not addressing the version requested", len(empty.Entries))
	}
	if _, err := s.Pin(len(s.History())); err == nil {
		t.Error("Pin accepted a version that does not exist")
	}

	pinned, err := s.Pin(installed.N)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revoke("r", "gone"); err != nil {
		t.Fatal(err)
	}

	// A replay running against the pinned version still sees the world
	// it was pinned to; otherwise it silently measures a moving target.
	if _, ok := pinned.Lookup("r"); !ok {
		t.Error("a pinned version changed under a later write")
	}
	if _, ok := s.Current().Lookup("r"); ok {
		t.Error("the current version still holds the revoked rule")
	}

	back, err := s.Rollback(installed.N)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Lookup("r"); !ok {
		t.Error("rollback did not restore the rule")
	}
	// History is append-only: the rollback is a new version, not an
	// erasure of the one it undid.
	if back.N <= installed.N {
		t.Errorf("rollback produced version %d, expected a new one after %d", back.N, installed.N)
	}
	if len(s.History()) != back.N+1 {
		t.Errorf("history has %d versions, want %d", len(s.History()), back.N+1)
	}
}

func TestAblateLeavesTheStoreAlone(t *testing.T) {
	s := mustStore(t, nil)
	for _, id := range []string{"a", "b"} {
		propose(t, s, id, "proposer", "p1", goodEvidence())
		if _, err := s.Install(&ladder.InstallArgs{ProposalID: id, Installer: "other", Pass: "p2", Stage: ladder.StageDeterministic}); err != nil {
			t.Fatal(err)
		}
	}

	without := s.Ablate("a")
	if len(without) != 1 || without[0].RuleID != "b" {
		t.Errorf("Ablate = %+v, want just b", without)
	}
	if len(s.Current().Entries) != 2 {
		t.Error("Ablate mutated the store; it must only produce a candidate world to replay against")
	}
}

func TestProposalsMustCarryWhatMakesThemAuditable(t *testing.T) {
	s := mustStore(t, nil)
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

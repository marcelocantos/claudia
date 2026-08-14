// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// narrowRule covers the routine case and nothing else, so a version
// holding it is measurably different from one that does not.
func narrowRule(id string) ladder.RuleDef {
	return ladder.RuleDef{
		ID:          id,
		Description: "retire a finished worker",
		When:        []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.finished"}},
		Answer:      "reaped",
	}
}

// installRule runs a full two-pass consolidation and returns the version
// the install produced.
func installRule(t *testing.T, s *ladder.RuleSet, id string, body ladder.RuleDef, stage ladder.Stage) ladder.Version {
	t.Helper()
	if err := s.Propose(&ladder.Proposal{
		ID:          id,
		Class:       "worker.finished",
		Description: body.Description,
		Rule:        body,
		ProposedBy:  "working-pass",
		Pass:        "work",
		Evidence:    goodEvidence(),
	}); err != nil {
		t.Fatalf("Propose(%s): %v", id, err)
	}
	if err := s.Install(&ladder.InstallArgs{
		ProposalID: id, Installer: "consolidation-pass", Pass: "consolidate", Stage: stage,
	}); err != nil {
		t.Fatalf("Install(%s): %v", id, err)
	}
	return s.Pin()
}

// ladderAt builds the ladder as it stood at one pinned version: a
// pattern rung holding that version's rules, below the model rung. The
// bodies are decoded here rather than in the store, because claudia
// carries a rule body it does not interpret.
func ladderAt(t *testing.T, f *fixture, v ladder.Version) *ladder.Ladder {
	t.Helper()
	var rules []ladder.RuleDef
	for _, r := range v.Rules() {
		var def ladder.RuleDef
		if err := r.Decode(&def); err != nil {
			t.Fatalf("decoding rule %q: %v", r.RuleID, err)
		}
		rules = append(rules, def)
	}
	if len(rules) == 0 {
		return ladder.New(f.modelRung(t))
	}
	return ladder.New(f.patternLayer(t, rules), f.modelRung(t))
}

// Every change to the rule set is a version, and the history reads as
// the sequence of acts that produced the current state rather than a
// series of unexplained snapshots.
func TestEveryChangeIsAVersionAndTheHistoryReadsAsActs(t *testing.T) {
	s := mustRuleSet(t, nil)

	seed := s.Pin()
	if seed.Verb != ladder.VerbSeed {
		t.Errorf("first version verb = %q, want %q", seed.Verb, ladder.VerbSeed)
	}
	if len(seed.Rules()) != 0 {
		t.Errorf("an empty set seeded with %d rules", len(seed.Rules()))
	}

	installed := installRule(t, s, "reap-finished", narrowRule("reap-finished"), ladder.StageDeterministic)
	if installed.Verb != ladder.VerbInstall || installed.RuleID != "reap-finished" {
		t.Errorf("install version = %+v", installed)
	}
	if installed.Fingerprint == seed.Fingerprint {
		t.Error("installing a rule did not move the fingerprint")
	}

	if _, err := s.Fail("reap-finished", ladder.FailureSafety, "reaped a worker that had not reported"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	demoted := s.Pin()
	if demoted.Verb != ladder.VerbDemote || demoted.RuleID != "reap-finished" {
		t.Errorf("demote version = %+v", demoted)
	}
	// A rule that fell explains why, without anyone reconstructing the
	// pass it fell in.
	if !strings.Contains(demoted.Reason, string(ladder.FailureSafety)) {
		t.Errorf("demotion reason %q does not name the failure that caused it", demoted.Reason)
	}

	if err := s.Forget("reap-finished", "superseded"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if got := s.Pin(); got.Verb != ladder.VerbForget {
		t.Errorf("forget version = %+v", got)
	}

	history := s.Versions()
	if len(history) != 4 {
		t.Fatalf("history has %d versions, want 4 (seed, install, demote, forget)", len(history))
	}
	want := []ladder.Verb{ladder.VerbSeed, ladder.VerbInstall, ladder.VerbDemote, ladder.VerbForget}
	for i, verb := range want {
		if history[i].Verb != verb {
			t.Errorf("history[%d] = %q, want %q — the history is oldest first and append-only", i, history[i].Verb, verb)
		}
	}
	// Forgetting the only rule returns the contents to the seed state,
	// and the fingerprint is over content, so it comes back the same.
	if history[3].Fingerprint != seed.Fingerprint {
		t.Errorf("returning to the same rules gave a different fingerprint: %s vs %s", history[3].Fingerprint, seed.Fingerprint)
	}

	// The store's past is not the caller's to edit: an audit asks what
	// was in force when a decision was made, and every rule in the set
	// claims to have been promoted on evidence.
	history[0].Verb = "tampered"
	if s.Versions()[0].Verb != ladder.VerbSeed {
		t.Error("a caller edited the store's history through the slice it was handed")
	}
}

// A set restored from an earlier run has a pin to hand before it has
// learned anything, so a replay against a freshly loaded store can name
// the version it ran against.
func TestASeededSetIsAVersionLikeAnyOther(t *testing.T) {
	first := mustRuleSet(t, nil)
	installRule(t, first, "reap-finished", narrowRule("reap-finished"), ladder.StageDeterministic)

	saved, err := first.Save()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	restored, err := ladder.LoadRuleSet("test", saved, nil)
	if err != nil {
		t.Fatalf("LoadRuleSet: %v", err)
	}

	pin := restored.Pin()
	if pin.Verb != ladder.VerbSeed {
		t.Errorf("restored pin verb = %q, want %q", pin.Verb, ladder.VerbSeed)
	}
	if pin.Fingerprint != first.Pin().Fingerprint {
		t.Errorf("restored fingerprint %s does not match the run it came from (%s)", pin.Fingerprint, first.Pin().Fingerprint)
	}
	if len(pin.Rules()) != 1 {
		t.Errorf("restored version holds %d rules, want 1", len(pin.Rules()))
	}
}

func TestRollbackRestoresContentAndIsItselfRecorded(t *testing.T) {
	s := mustRuleSet(t, nil)
	installRule(t, s, "reap-finished", narrowRule("reap-finished"), ladder.StageDeterministic)
	two := installRule(t, s, "reap-completed", narrowRule("reap-completed"), ladder.StageDeterministic)

	if err := s.Forget("reap-completed", "looked inert against the corpus"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if len(s.Rules()) != 1 {
		t.Fatalf("the store holds %d rules after forgetting one of two", len(s.Rules()))
	}

	if err := s.Rollback(two.Fingerprint, "the ablation was measured against the wrong corpus"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := len(s.Rules()); got != 2 {
		t.Errorf("rollback restored %d rules, want 2", got)
	}
	// The fingerprint is over content, so a replay pinned to the
	// rolled-back version is directly comparable with the one that ran
	// when those rules were first in force.
	fp, err := s.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fp != two.Fingerprint {
		t.Errorf("rolled-back contents fingerprint as %s, want %s", fp, two.Fingerprint)
	}

	// The history moves FORWARD. That a rollback happened, and why, is
	// itself part of the record.
	pin := s.Pin()
	if pin.Verb != ladder.VerbRollback {
		t.Errorf("pin verb = %q, want %q", pin.Verb, ladder.VerbRollback)
	}
	if !strings.Contains(pin.Reason, "wrong corpus") {
		t.Errorf("rollback reason = %q", pin.Reason)
	}
	if len(s.Versions()) != 5 {
		t.Errorf("history has %d versions, want 5 — a rollback appends, it does not rewind", len(s.Versions()))
	}

	if err := s.Rollback("sha256:neverheldthis", "typo"); !errors.Is(err, ladder.ErrNoSuchVersion) {
		t.Errorf("err = %v, want ErrNoSuchVersion", err)
	}
}

// The named oracle for pinning: two replays of ONE recording against
// different pinned store versions produce different results, and each
// report carries the version it ran against.
//
// Without the pin the two runs are indistinguishable after the fact —
// behaviour is history-dependent once rules accumulate, so an
// unidentified replay is silently measuring a moving target and its
// numbers cannot be compared with anything.
func TestReplaysAtDifferentPinnedVersionsDifferAndSayWhichIsWhich(t *testing.T) {
	f := newFixture()
	s := mustRuleSet(t, nil)

	empty := s.Pin()
	withRule := installRule(t, s, "reap-finished", narrowRule("reap-finished"), ladder.StageDeterministic)
	if empty.Fingerprint == withRule.Fingerprint {
		t.Fatal("the two versions are the same; there is nothing to measure")
	}

	// One recording, two worlds. The ladder for each run is built from
	// the pinned version's own rules, so the report cannot be attributed
	// to a version it did not run against.
	before := replayAt(t, ladderAt(t, f, empty), empty.Fingerprint)
	after := replayAt(t, ladderAt(t, f, withRule), withRule.Fingerprint)

	if before.Fingerprint != empty.Fingerprint || after.Fingerprint != withRule.Fingerprint {
		t.Fatalf("reports are pinned to %s and %s, want %s and %s",
			before.Fingerprint, after.Fingerprint, empty.Fingerprint, withRule.Fingerprint)
	}
	if before.Stats.ModelShare() != 1 {
		t.Errorf("model share at the empty version = %.2f, want 1", before.Stats.ModelShare())
	}
	if after.Stats.ModelShare() >= before.Stats.ModelShare() {
		t.Errorf("the rule changed nothing: model share %.2f then %.2f",
			before.Stats.ModelShare(), after.Stats.ModelShare())
	}

	cmp, err := ladder.CompareReplays(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Before != empty.Fingerprint || cmp.After != withRule.Fingerprint {
		t.Errorf("comparison attributes %s→%s, want %s→%s", cmp.Before, cmp.After, empty.Fingerprint, withRule.Fingerprint)
	}
	if !cmp.Cheaper || cmp.OversightRegression {
		t.Errorf("a sound promotion read as %s", cmp.Summary())
	}

	// And a rollback lands somewhere the earlier measurement is still
	// comparable, because the fingerprint follows content rather than
	// position in the history.
	if err := s.Rollback(empty.Fingerprint, "the promotion was premature"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	rolled := s.Pin()
	if rolled.Fingerprint != empty.Fingerprint {
		t.Fatalf("rolled-back pin = %s, want %s", rolled.Fingerprint, empty.Fingerprint)
	}
	again := replayAt(t, ladderAt(t, f, rolled), rolled.Fingerprint)
	if again.Stats.ModelShare() != before.Stats.ModelShare() {
		t.Errorf("replay after rollback = %.2f, want the pre-promotion %.2f",
			again.Stats.ModelShare(), before.Stats.ModelShare())
	}
	if _, err := ladder.CompareReplays(before, again); err == nil {
		t.Error("two reports at the SAME version were compared; a version always looks stable against itself")
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

func learned(id, class string, ev ladder.Evidence) ladder.Recalled {
	return ladder.Recalled{
		RuleID: id, Class: class,
		Description: "retire a worker that has finished and reported",
		Stage:       ladder.StageDeterministic,
		Evidence:    ev,
	}
}

func TestSerialisationIsByteIdenticalAndSorted(t *testing.T) {
	rules := []ladder.Recalled{
		learned("zeta", "c", goodEvidence()),
		learned("alpha", "c", goodEvidence()),
		learned("mid", "c", goodEvidence()),
	}

	first, err := ladder.MarshalRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	// Same set, different input order: the output must not move, or a
	// git diff shows noise instead of change.
	shuffled := []ladder.Recalled{rules[1], rules[2], rules[0]}
	second, err := ladder.MarshalRules(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("serialisation is order-dependent:\n%s\n---\n%s", first, second)
	}

	// Sorted by rule id, so the file reads predictably.
	body := string(first)
	if strings.Index(body, "alpha") > strings.Index(body, "mid") ||
		strings.Index(body, "mid") > strings.Index(body, "zeta") {
		t.Errorf("entries are not sorted by rule id:\n%s", body)
	}
}

func TestFingerprintTracksContentAndNothingElse(t *testing.T) {
	rules := []ladder.Recalled{learned("a", "c", goodEvidence())}

	fp1, err := ladder.Fingerprint(rules)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := ladder.Fingerprint([]ladder.Recalled{learned("a", "c", goodEvidence())})
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Error("identical rule sets fingerprinted differently")
	}

	changed, err := ladder.Fingerprint([]ladder.Recalled{learned("a", "different-class", goodEvidence())})
	if err != nil {
		t.Fatal(err)
	}
	if changed == fp1 {
		t.Error("a changed rule set kept its fingerprint")
	}
	if !strings.HasPrefix(fp1, "sha256:") {
		t.Errorf("fingerprint = %q, want a content hash", fp1)
	}
}

func TestSeededMemoryRoundTripsThroughYAML(t *testing.T) {
	original, err := ladder.NewRuleSet("po-flavour", []ladder.Recalled{
		learned("reap-finished", "worker.finished", goodEvidence()),
		learned("nudge-idle", "worker.idle", goodEvidence()),
	})
	if err != nil {
		t.Fatal(err)
	}

	saved, err := original.Save()
	if err != nil {
		t.Fatal(err)
	}

	// A stack restarting picks the file back up.
	restored, err := ladder.LoadRuleSet("po-flavour", saved)
	if err != nil {
		t.Fatalf("LoadRuleSet: %v", err)
	}

	wantFP, _ := original.Fingerprint()
	gotFP, _ := restored.Fingerprint()
	if wantFP != gotFP {
		t.Errorf("a round trip changed the rule set: %s vs %s", wantFP, gotFP)
	}
	if len(restored.Rules()) != 2 {
		t.Fatalf("restored %d rules, want 2", len(restored.Rules()))
	}
	if restored.Rules()[0].Stage != ladder.StageDeterministic {
		t.Error("stage did not survive the round trip")
	}
	if restored.Rules()[0].Evidence.Runs != goodEvidence().Runs {
		t.Error("evidence did not survive the round trip")
	}
}

func TestMalformedRulesRefuseRatherThanStartingEmpty(t *testing.T) {
	// An empty memory is indistinguishable from one that never learned
	// anything, and the ladder would quietly revert to waking a model
	// for everything while reporting perfect health.
	for name, data := range map[string]string{
		"not yaml":  "\t\x00 this is not yaml: [",
		"no id":     "- description: d\n  stage: deterministic\n",
		"no stage":  "- rule_id: r\n  description: d\n",
		"wrong top": "rule_id: r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ladder.LoadRuleSet("f", []byte(data)); err == nil {
				t.Error("malformed input loaded as an empty memory")
			}
		})
	}

	if _, err := ladder.NewRuleSet("", nil); err == nil {
		t.Error("a rule set with no flavour was accepted")
	}
	if _, err := ladder.NewRuleSet("f", []ladder.Recalled{learned("dup", "c", goodEvidence()), learned("dup", "c", goodEvidence())}); err == nil {
		t.Error("a seed naming one rule twice was accepted")
	}
}

// The consequence of sharing a rule set across a flavour: two stacks can
// hold different scopes, and the same rule must be legitimate in one and
// refused in the other.
func TestAttachValidatesPerStackNotOncePerSet(t *testing.T) {
	f := newFixture()
	shared, err := ladder.NewRuleSet("po-flavour", []ladder.Recalled{
		learned("spawn-replacement", "worker.finished", goodEvidence()),
	})
	if err != nil {
		t.Fatal(err)
	}
	actionsOf := func(r *ladder.Recalled) ([]string, error) { return []string{"agent.spawn"}, nil }

	wide := f.scope(t, "wide", nil, []ladder.Action{f.reap, f.spawn})
	narrow := f.scope(t, "narrow", nil, []ladder.Action{f.reap})

	if _, err := shared.Attach(wide, actionsOf); err != nil {
		t.Errorf("a stack that may spawn was refused: %v", err)
	}

	_, err = shared.Attach(narrow, actionsOf)
	if err == nil {
		t.Fatal("a reap-only stack attached a rule that spawns; authority leaked between stacks")
	}
	if !strings.Contains(err.Error(), "agent.spawn") || !strings.Contains(err.Error(), "narrow") {
		t.Errorf("refusal does not name the rule and the layer: %v", err)
	}

	// Attach is a snapshot: a later change does not reach an attachment
	// already made, so a running ladder never changes behaviour without
	// re-passing the load-time gate.
	att, err := shared.Attach(wide, actionsOf)
	if err != nil {
		t.Fatal(err)
	}
	before := att.Fingerprint
	if err := shared.Apply([]ladder.Recalled{learned("something-else", "c", goodEvidence())}); err != nil {
		t.Fatal(err)
	}
	if att.Fingerprint != before || len(att.Rules) != 1 || att.Rules[0].RuleID != "spawn-replacement" {
		t.Error("an existing attachment changed under a live rule-set update")
	}

	after, err := shared.Attach(wide, actionsOf)
	if err != nil {
		t.Fatal(err)
	}
	if after.Fingerprint == before {
		t.Error("re-attaching did not pick up the new rules")
	}
}

func TestJournalRecordsReplayableEpisodesAndLateLabels(t *testing.T) {
	f := newFixture()
	s := f.scope(t, "rung", []ladder.Read{f.status}, []ladder.Action{f.reap})
	l := ladder.New(ladder.NewLayerFunc(s, func(ctx context.Context, req *ladder.Request, rd *ladder.Reader) (*ladder.Verdict, error) {
		if _, err := rd.Do(ctx, f.status, req.Payload); err != nil {
			return nil, err
		}
		return &ladder.Verdict{Kind: ladder.VerdictAct, Rule: "r1", Action: f.reap}, nil
	}))

	req := &ladder.Request{Kind: "worker.finished", Payload: "jv-1"}
	res, err := l.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	j := ladder.NewMemoryJournal(0)
	ep := ladder.EpisodeFrom("ep-1", "po-a", "sha256:abc", req, res)
	if err := j.Record(ep); err != nil {
		t.Fatal(err)
	}

	got := j.Episodes()
	if len(got) != 1 {
		t.Fatalf("episodes = %d", len(got))
	}
	// The reads are what make it re-runnable rather than a statistic.
	if len(got[0].Reads) != 1 || got[0].Reads[0].Name != "agent.status" {
		t.Errorf("the answering rung's reads were not captured: %+v", got[0].Reads)
	}
	if got[0].Rule != "r1" || got[0].AnsweredBy != "rung" || got[0].Stack != "po-a" {
		t.Errorf("provenance incomplete: %+v", got[0])
	}
	if got[0].Outcome != nil {
		t.Error("an unjudged episode carries an outcome")
	}

	// The label arrives hours later, as a second write.
	if err := j.Judge("ep-1", &ladder.Outcome{Delivered: true, Note: "worker retired cleanly"}); err != nil {
		t.Fatal(err)
	}
	if out := j.Episodes()[0].Outcome; out == nil || !out.Delivered {
		t.Errorf("the late label did not attach: %+v", out)
	}

	// A label for an episode that has already decayed is not an error;
	// it simply arrived after its evidence expired.
	if err := j.Judge("never-existed", &ladder.Outcome{Delivered: true}); err != nil {
		t.Errorf("a label for a decayed episode errored: %v", err)
	}
	if err := j.Record(&ladder.Episode{}); err == nil {
		t.Error("an episode with no id was recorded; a later outcome would have nothing to attach to")
	}
}

func TestJournalDecaysAndRetentionIsDerived(t *testing.T) {
	j := ladder.NewMemoryJournal(3)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if err := j.Record(&ladder.Episode{ID: id, Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	got := j.Episodes()
	if len(got) != 3 || got[0].ID != "c" || got[2].ID != "e" {
		t.Errorf("retention kept %d episodes (%v); episodic memory is supposed to decay, oldest first", len(got), got)
	}
	// The index survives eviction, so a late label still lands.
	if err := j.Judge("e", &ladder.Outcome{Delivered: true}); err != nil {
		t.Fatal(err)
	}
	if j.Episodes()[2].Outcome == nil {
		t.Error("eviction broke the correlation index")
	}

	// The floor is computed from the thresholds rather than picked. A
	// class that is one request in twenty needs twenty times the gate.
	th := ladder.ProductionThresholds()
	if floor := ladder.RetentionFloor(th, 0.05, 1); floor != th.HybridToDeterministicRuns*20 {
		t.Errorf("RetentionFloor = %d, want %d", floor, th.HybridToDeterministicRuns*20)
	}
	if ladder.RetentionFloor(th, 1, 2) != th.HybridToDeterministicRuns*2 {
		t.Error("margin is not applied")
	}
}

func TestNopJournalRecordsNothing(t *testing.T) {
	var j ladder.Journal = ladder.NopJournal{}
	if err := j.Record(&ladder.Episode{ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Judge("x", &ladder.Outcome{}); err != nil {
		t.Fatal(err)
	}
	if len(j.Episodes()) != 0 {
		t.Error("the no-op journal retained something")
	}
}

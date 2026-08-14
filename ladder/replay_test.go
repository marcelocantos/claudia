// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// corpus is a recorded request mix: two thirds are the routine case a
// rule can learn, one third is the case that genuinely needs judgement.
func corpus() []*ladder.Request {
	var reqs []*ladder.Request
	for range 20 {
		reqs = append(reqs, &ladder.Request{Kind: "worker.finished", Payload: "jv-routine"})
		reqs = append(reqs, &ladder.Request{Kind: "worker.finished", Payload: "jv-routine"})
		reqs = append(reqs, &ladder.Request{Kind: "worker.blocked", Payload: "jv-hard"})
	}
	return reqs
}

// modelRung answers everything, which is what the top of a ladder does.
func (f *fixture) modelRung(t *testing.T) ladder.Layer {
	t.Helper()
	s := f.scope(t, "model", nil, nil)
	return ladder.NewLayerFunc(s, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
		return &ladder.Verdict{Kind: ladder.VerdictAnswer, Answer: "handled"}, nil
	})
}

// delivered is the consumer's judgement: the blocked case is only
// handled properly when a model saw it.
func delivered(req *ladder.Request, res *ladder.Result) bool {
	if res == nil || res.Verdict == nil {
		return false
	}
	if req.Kind == "worker.blocked" {
		return res.Layer == "model"
	}
	return true
}

func replayAt(t *testing.T, l *ladder.Ladder, fingerprint string) *ladder.ReplayReport {
	t.Helper()
	rep, err := ladder.Replay(context.Background(), &ladder.ReplayArgs{
		Ladder:      l,
		Fingerprint: fingerprint,
		Corpus:      corpus(),
		ModelLayers: []string{"model"},
		Delivered:   delivered,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return rep
}

func TestReplayNeedsAnIdentifiedRuleSetAndAWorkJudgement(t *testing.T) {
	f := newFixture()
	l := ladder.New(f.modelRung(t))

	if _, err := ladder.Replay(context.Background(), &ladder.ReplayArgs{
		Ladder: l, Corpus: corpus(), Delivered: delivered,
	}); err == nil {
		t.Error("an unidentified replay was accepted; it would be measuring a moving target")
	}
	if _, err := ladder.Replay(context.Background(), &ladder.ReplayArgs{
		Ladder: l, Fingerprint: "sha256:empty", Corpus: corpus(),
	}); err == nil {
		t.Error("a replay measuring cost with no delivered-work judgement was accepted")
	}
}

// A good promotion: the routine case moves below the model and the hard
// case still escalates. Cheaper, with delivered work untouched.
func TestGoodPromotionIsCheaperWithoutLosingWork(t *testing.T) {
	f := newFixture()
	rs := mustRuleSet(t, nil)

	before := replayAt(t, ladder.New(f.modelRung(t)), "sha256:empty")
	if before.Stats.ModelShare() != 1 {
		t.Fatalf("baseline model share = %.2f, want 1", before.Stats.ModelShare())
	}

	// Install a rule covering only the routine case.
	propose(t, rs, "reap-finished", "proposer", "p1", goodEvidence())
	if err := rs.Install(&ladder.InstallArgs{
		ProposalID: "reap-finished", Installer: "consolidator", Pass: "p2", Stage: ladder.StageDeterministic,
	}); err != nil {
		t.Fatal(err)
	}
	v, err := rs.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	narrow := f.patternLayer(t, []ladder.RuleDef{{
		ID: "reap-finished", Description: "retire a finished worker",
		When:   []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.finished"}},
		Answer: "reaped",
	}})
	after := replayAt(t, ladder.New(narrow, f.modelRung(t)), v)

	cmp, err := ladder.CompareReplays(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Cheaper {
		t.Errorf("promotion was not cheaper: %s", cmp.Summary())
	}
	if cmp.OversightRegression {
		t.Errorf("a sound promotion was flagged as a regression: %s", cmp.Summary())
	}
	if cmp.DeliveredShareDelta != 0 {
		t.Errorf("delivered work moved: %s", cmp.Summary())
	}
	if !strings.Contains(cmp.Summary(), "cheaper at no cost") {
		t.Errorf("summary = %q", cmp.Summary())
	}
}

// The failure this architecture exists to make visible. A rule that
// handles everything and escalates nothing is the cheapest expressible
// rule, it looks like a triumph on cost alone, and it is silently
// answering the case that needed judgement.
func TestGreedyRulePresentsAsASavingAndIsCaught(t *testing.T) {
	f := newFixture()
	rs := mustRuleSet(t, nil)

	// The baseline is a ladder that ALREADY has a sound rule, because
	// the escalation-rate signal only means anything within a fixed
	// ladder shape: a one-rung ladder cannot escalate, so comparing
	// against it would show no collapse however greedy the new rule is.
	// What is being measured here is a rule set getting greedier across
	// store versions, which is the shape the real failure takes.
	narrow := f.patternLayer(t, []ladder.RuleDef{{
		ID: "reap-finished", Description: "retire a finished worker",
		When:   []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.finished"}},
		Answer: "reaped",
	}})
	before := replayAt(t, ladder.New(narrow, f.modelRung(t)), "sha256:empty")

	propose(t, rs, "handle-everything", "proposer", "p1", goodEvidence())
	if err := rs.Install(&ladder.InstallArgs{
		ProposalID: "handle-everything", Installer: "consolidator", Pass: "p2", Stage: ladder.StageDeterministic,
	}); err != nil {
		t.Fatal(err)
	}
	v, err := rs.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	greedy := f.patternLayer(t, []ladder.RuleDef{{
		ID: "handle-everything", Description: "handle everything",
		When:   []ladder.Predicate{{Field: "kind", Kind: ladder.Present}},
		Answer: "handled",
	}})
	after := replayAt(t, ladder.New(greedy, f.modelRung(t)), v)

	cmp, err := ladder.CompareReplays(before, after)
	if err != nil {
		t.Fatal(err)
	}

	// On cost alone it is the best result available: nothing reaches a
	// model at all.
	if after.Stats.ModelShare() != 0 || !cmp.Cheaper {
		t.Fatalf("greedy rule did not look like a saving: %s", cmp.Summary())
	}
	// And it is a regression, because the work stopped happening.
	if !cmp.OversightRegression {
		t.Errorf("silent omission was not caught: %s", cmp.Summary())
	}
	if !strings.Contains(cmp.Summary(), "OVERSIGHT REGRESSION") {
		t.Errorf("summary buries the finding: %q", cmp.Summary())
	}
	// The escalation rate collapsing is the leading indicator.
	if cmp.EscalationRateDelta >= 0 {
		t.Errorf("escalation rate did not fall: %s", cmp.Summary())
	}
}

func TestComparisonRefusesMeaninglessPairs(t *testing.T) {
	f := newFixture()
	rep := replayAt(t, ladder.New(f.modelRung(t)), "sha256:empty")

	if _, err := ladder.CompareReplays(rep, rep); err == nil {
		t.Error("a report was compared against itself; a rule set always looks stable against itself")
	}

	short, err := ladder.Replay(context.Background(), &ladder.ReplayArgs{
		Ladder: ladder.New(f.modelRung(t)), Fingerprint: "sha256:different",
		Corpus: corpus()[:3], ModelLayers: []string{"model"}, Delivered: delivered,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ladder.CompareReplays(rep, short); err == nil {
		t.Error("reports over different corpora were compared")
	}
}

func TestMeterSeparatesCostFromOversightAndFromDefects(t *testing.T) {
	f := newFixture()
	broken := f.scope(t, "broken", nil, nil)
	m := ladder.NewMeter(&ladder.MeterConfig{ModelLayers: []string{"model"}})

	l := ladder.New(
		ladder.NewLayerFunc(broken, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
			return nil, nil // contract violation
		}),
		f.modelRung(t),
	)

	for _, req := range []*ladder.Request{{Kind: "k"}, {Kind: "k"}} {
		res, err := l.Evaluate(context.Background(), req)
		m.Observe(&ladder.Observation{Request: req, Result: res, Err: err, Delivered: true})
	}

	c := m.Class("k")
	if c.Requests != 2 || c.ReachedModel != 2 {
		t.Errorf("stats = %+v", c)
	}
	if c.Defects != 2 {
		t.Errorf("defects = %d, want 2 — a rung passing through silently must be counted", c.Defects)
	}
	if c.EscalationRate() != 1 || c.ModelShare() != 1 || c.DeliveredShare() != 1 {
		t.Errorf("rates: escalation %.2f model %.2f delivered %.2f", c.EscalationRate(), c.ModelShare(), c.DeliveredShare())
	}

	// An unanswered request is counted as such rather than vanishing.
	empty := &ladder.Request{Kind: "unanswerable"}
	res, err := ladder.New().Evaluate(context.Background(), empty)
	m.Observe(&ladder.Observation{Request: empty, Result: res, Err: err})
	if u := m.Class("unanswerable"); u.Unanswered != 1 || u.Delivered != 0 {
		t.Errorf("unanswered stats = %+v", u)
	}

	if totals := m.Totals(); totals.Requests != 3 {
		t.Errorf("totals = %+v", totals)
	}
	if got := m.Classes(); len(got) != 2 || got[0] != "k" {
		t.Errorf("Classes() = %v", got)
	}
}

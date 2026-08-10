// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// scanFixture installs one live rule and one that matches an event that
// no longer occurs, then scans.
type scanFixture struct {
	f     *fixture
	store *ladder.Store
	args  *ladder.SymptomScan
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := newFixture()
	store := ladder.NewStore(nil)

	install := func(id string, rule ladder.RuleDef, ev ladder.Evidence) {
		t.Helper()
		if err := store.Propose(&ladder.Proposal{
			ID: id, Class: "worker.finished", Description: rule.Description,
			Rule: rule, ProposedBy: "work", Pass: "p1", Evidence: ev,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Install(&ladder.InstallArgs{
			ProposalID: id, Installer: "consolidate", Pass: "p2", Stage: ladder.StageDeterministic,
		}); err != nil {
			t.Fatal(err)
		}
	}

	install("live", ladder.RuleDef{
		ID: "live", Description: "retire a finished worker",
		When:   []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.finished"}},
		Answer: "reaped",
	}, goodEvidence())

	// Promoted on repetition alone, and for an event that stopped
	// happening. It never errors and looks exactly as healthy as the
	// live one.
	install("stale", ladder.RuleDef{
		ID: "stale", Description: "an event that no longer occurs",
		When:   []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.vanished"}},
		Answer: "gone",
	}, ladder.Evidence{Runs: 60, Identical: 60, TestsPass: true})

	sf := &scanFixture{f: f, store: store}
	sf.args = &ladder.SymptomScan{
		Store:       store,
		Corpus:      corpus(),
		ModelLayers: []string{"model"},
		Delivered:   delivered,
		Build: func(entries []ladder.Entry) (*ladder.Ladder, error) {
			var rules []ladder.RuleDef
			for _, e := range entries {
				if def, ok := e.Rule.(ladder.RuleDef); ok {
					rules = append(rules, def)
				}
			}
			if len(rules) == 0 {
				return ladder.New(f.modelRung(t)), nil
			}
			pattern, err := ladder.NewPatternLayer(&ladder.PatternConfig{
				Scope:  f.scope(t, "pattern", nil, []ladder.Action{f.reap}),
				Fields: requestFields(),
				Rules:  rules,
			})
			if err != nil {
				return nil, err
			}
			return ladder.New(pattern, f.modelRung(t)), nil
		},
	}
	return sf
}

func find(symptoms []ladder.Symptom, kind ladder.SymptomKind, ruleID string) *ladder.Symptom {
	for i := range symptoms {
		if symptoms[i].Kind == kind && symptoms[i].RuleID == ruleID {
			return &symptoms[i]
		}
	}
	return nil
}

func TestScanSeparatesInertFromLoadBearing(t *testing.T) {
	sf := newScanFixture(t)
	symptoms, err := ladder.Scan(context.Background(), sf.args)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// The stale rule is inert AND never fired — two independent
	// findings, because a rule can fire and still change nothing.
	if s := find(symptoms, ladder.SymptomInert, "stale"); s == nil {
		t.Error("the inert rule was not identified")
	}
	if s := find(symptoms, ladder.SymptomNeverFired, "stale"); s == nil {
		t.Error("a rule matching nothing was not reported")
	}

	// The live rule is load-bearing, and the ablation says by how much.
	s := find(symptoms, ladder.SymptomLoadBearing, "live")
	if s == nil {
		t.Fatal("the live rule was not identified as load-bearing")
	}
	if s.Observed <= 0 {
		t.Errorf("removing the live rule did not move model share: %+v", s)
	}
	if find(symptoms, ladder.SymptomInert, "live") != nil {
		t.Error("a load-bearing rule was also reported inert")
	}

	// Promotion on repetition alone is visible without recomputing the
	// judgement.
	if s := find(symptoms, ladder.SymptomConsistencyOnly, "stale"); s == nil {
		t.Error("a rule promoted on consistency alone was not flagged")
	}
	if find(symptoms, ladder.SymptomConsistencyOnly, "live") != nil {
		t.Error("a rule with correctness signal was flagged consistency-only")
	}
}

func TestScanReportsAndNeverActs(t *testing.T) {
	sf := newScanFixture(t)
	before := sf.store.Current()

	if _, err := ladder.Scan(context.Background(), sf.args); err != nil {
		t.Fatal(err)
	}

	// The store is exactly as it was. Symptoms are observations, never
	// conclusions: the runtime does not revoke a rule because one
	// looked stale.
	after := sf.store.Current()
	if after.N != before.N {
		t.Errorf("the scan changed the store from v%d to v%d", before.N, after.N)
	}
	if len(after.Entries) != 2 {
		t.Errorf("the scan removed a rule: %d entries remain", len(after.Entries))
	}
}

func TestScanSurfacesContradictionAndEscalationCollapse(t *testing.T) {
	sf := newScanFixture(t)

	// A correction is the correctness signal consistency cannot supply,
	// and it comes from the consumer.
	sf.args.Corrections = map[string]int{"live": 3}

	// A baseline where the class escalated far more than it does now.
	sf.args.Baseline = &ladder.ReplayReport{
		Version: 0,
		PerClass: map[string]ladder.ClassStats{
			"worker.finished": {Requests: 40, Escalated: 40, AnsweredBy: map[string]int{"model": 40}},
		},
	}

	symptoms, err := ladder.Scan(context.Background(), sf.args)
	if err != nil {
		t.Fatal(err)
	}

	s := find(symptoms, ladder.SymptomContradicted, "live")
	if s == nil || s.Observed != 3 {
		t.Errorf("contradiction not surfaced: %+v", s)
	}

	var collapse *ladder.Symptom
	for i := range symptoms {
		if symptoms[i].Kind == ladder.SymptomEscalationCollapse {
			collapse = &symptoms[i]
		}
	}
	if collapse == nil {
		t.Fatal("an escalation collapse was not reported")
	}
	if !strings.Contains(collapse.Detail, "optimising away its own oversight") {
		t.Errorf("the finding is stated as an efficiency number: %q", collapse.Detail)
	}
	if collapse.Baseline <= collapse.Observed {
		t.Errorf("collapse figures are the wrong way round: %+v", collapse)
	}

	// Without a baseline, no collapse is claimed rather than one being
	// invented from a single point.
	sf.args.Baseline = nil
	symptoms, err = ladder.Scan(context.Background(), sf.args)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range symptoms {
		if s.Kind == ladder.SymptomEscalationCollapse {
			t.Error("a collapse was reported with nothing to compare against")
		}
	}
}

func TestScanRefusesToRunWithoutWhatItCannotInfer(t *testing.T) {
	sf := newScanFixture(t)
	ctx := context.Background()

	cases := map[string]func(*ladder.SymptomScan){
		"no store":     func(a *ladder.SymptomScan) { a.Store = nil },
		"no build":     func(a *ladder.SymptomScan) { a.Build = nil },
		"no delivered": func(a *ladder.SymptomScan) { a.Delivered = nil },
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			args := *sf.args
			break_(&args)
			if _, err := ladder.Scan(ctx, &args); err == nil {
				t.Error("scan ran without it")
			}
		})
	}
	if _, err := ladder.Scan(ctx, nil); err == nil {
		t.Error("scan ran on nil args")
	}
}

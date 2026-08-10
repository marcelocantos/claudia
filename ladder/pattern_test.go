// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
	"github.com/marcelocantos/claudia/ladder/laddertest"
)

// requestFields is the consumer's declaration of what rules may match.
// Rules match these, never a stringified request.
func requestFields() map[string]ladder.FieldFunc {
	return map[string]ladder.FieldFunc{
		"kind": func(r *ladder.Request) (string, bool) { return r.Kind, r.Kind != "" },
		"agent": func(r *ladder.Request) (string, bool) {
			s, ok := r.Payload.(string)
			return s, ok
		},
	}
}

func (f *fixture) patternLayer(t *testing.T, rules []ladder.RuleDef) *ladder.PatternLayer {
	t.Helper()
	l, err := ladder.NewPatternLayer(&ladder.PatternConfig{
		Scope:  f.scope(t, "pattern", nil, []ladder.Action{f.reap}),
		Fields: requestFields(),
		Rules:  rules,
	})
	if err != nil {
		t.Fatalf("NewPatternLayer: %v", err)
	}
	return l
}

func TestPatternRuleActsAndExplainsItself(t *testing.T) {
	f := newFixture()
	l := f.patternLayer(t, []ladder.RuleDef{{
		ID:          "reap-finished",
		Description: "a worker that reported and holds no open mission is retired",
		When: []ladder.Predicate{
			{Field: "kind", Kind: ladder.Equals, Value: "worker.finished"},
		},
		Action: "agent.reap",
	}})

	res, err := ladder.New(l).Evaluate(context.Background(), &ladder.Request{Kind: "worker.finished", Payload: "jv-t1"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Output != "reaped" || f.reaped != 1 {
		t.Errorf("output = %v, reaped = %d", res.Output, f.reaped)
	}
	if res.Verdict.Rule != "reap-finished" {
		t.Errorf("rule = %q", res.Verdict.Rule)
	}
	// The prose it was minted with is the answer to "why did you do
	// that", with no model involved.
	if got := res.Verdict.Explain(l.Scope()); !strings.Contains(got, "holds no open mission") {
		t.Errorf("Explain() = %q", got)
	}
}

func TestConflictingRulesEscalateRatherThanPickingByOrder(t *testing.T) {
	f := newFixture()
	l := f.patternLayer(t, []ladder.RuleDef{
		{
			ID:          "reap-it",
			Description: "reap",
			When:        []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.idle"}},
			Action:      "agent.reap",
		},
		{
			ID:          "leave-it",
			Description: "leave alone",
			When:        []ladder.Predicate{{Field: "agent", Kind: ladder.HasPrefix, Value: "jv-"}},
			Answer:      "leave",
		},
	})

	req := &ladder.Request{Kind: "worker.idle", Payload: "jv-t1"}
	v, err := l.Evaluate(context.Background(), req, l.Scope().NewReader())
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != ladder.VerdictAbstain {
		t.Fatalf("kind = %s, want abstain — declaration order must not decide", v.Kind)
	}
	if !strings.Contains(v.Reason, "leave-it") || !strings.Contains(v.Reason, "reap-it") {
		t.Errorf("reason does not name the conflict: %q", v.Reason)
	}
	if f.reaped != 0 {
		t.Error("a conflicting match still acted")
	}
	// The ambiguity is escalated as data, not swallowed.
	if len(req.Notes) != 1 || req.Notes[0].Kind != "ambiguity" {
		t.Errorf("notes = %+v", req.Notes)
	}

	// Rules agreeing on the same outcome collapse instead of conflicting.
	agree := f.patternLayer(t, []ladder.RuleDef{
		{ID: "a", Description: "d", When: []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "x"}}, Answer: "same"},
		{ID: "b", Description: "d", When: []ladder.Predicate{{Field: "agent", Kind: ladder.Present}}, Answer: "same"},
	})
	v, err = agree.Evaluate(context.Background(), &ladder.Request{Kind: "x", Payload: "a"}, agree.Scope().NewReader())
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != ladder.VerdictAnswer || v.Answer != "same" {
		t.Errorf("agreeing rules did not collapse: %+v", v)
	}
}

func TestPatternLoadRejectsWhatWouldFailQuietly(t *testing.T) {
	f := newFixture()
	scope := f.scope(t, "pattern", nil, []ladder.Action{f.reap})

	cases := map[string]ladder.RuleDef{
		"undeclared field": {
			ID: "r", When: []ladder.Predicate{{Field: "nope", Kind: ladder.Present}},
		},
		"action outside the manifest": {
			ID:     "r",
			When:   []ladder.Predicate{{Field: "kind", Kind: ladder.Present}},
			Action: "agent.spawn",
		},
		"unregistered action": {
			ID:     "r",
			When:   []ladder.Predicate{{Field: "kind", Kind: ladder.Present}},
			Action: "agent.nope",
		},
		"no conditions": {
			ID: "r",
		},
		"empty one_of": {
			ID: "r", When: []ladder.Predicate{{Field: "kind", Kind: ladder.OneOf}},
		},
		"unknown predicate kind": {
			ID: "r", When: []ladder.Predicate{{Field: "kind", Kind: "vibes"}},
		},
		"uncompilable pattern": {
			ID: "r", When: []ladder.Predicate{{Field: "kind", Kind: ladder.Matches, Value: "("}},
		},
		"lookaround is not RE2": {
			ID: "r", When: []ladder.Predicate{{Field: "kind", Kind: ladder.Matches, Value: "(?=x)"}},
		},
	}

	for name, rule := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ladder.NewPatternLayer(&ladder.PatternConfig{
				Scope: scope, Fields: requestFields(), Rules: []ladder.RuleDef{rule},
			})
			if err == nil {
				t.Error("load accepted a rule that would fail quietly at evaluation time")
			}
		})
	}

	dup := ladder.RuleDef{ID: "same", When: []ladder.Predicate{{Field: "kind", Kind: ladder.Present}}}
	if _, err := ladder.NewPatternLayer(&ladder.PatternConfig{
		Scope: scope, Fields: requestFields(), Rules: []ladder.RuleDef{dup, dup},
	}); err == nil {
		t.Error("load accepted a duplicate rule ID")
	}
}

func TestRuleSetIsAnalysableWithoutExecution(t *testing.T) {
	f := newFixture()
	l := f.patternLayer(t, []ladder.RuleDef{
		{ID: "idle", Description: "d", When: []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.idle"}}, Answer: 1},
		{ID: "finished", Description: "d", When: []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.finished"}}, Answer: 2},
		{ID: "any-jv", Description: "d", When: []ladder.Predicate{{Field: "agent", Kind: ladder.HasPrefix, Value: "jv-"}}, Answer: 3},
	})

	// idle and finished are provably disjoint: same field, different
	// exact values. any-jv constrains a different field, so it cannot be
	// proven separate from either, and the analysis says so rather than
	// claiming a separation it has not established.
	overlaps := l.Overlaps()
	if len(overlaps) != 2 {
		t.Fatalf("overlaps = %+v, want 2", overlaps)
	}
	for _, o := range overlaps {
		if o.A == "idle" && o.B == "finished" {
			t.Error("exact values on one field were reported as overlapping")
		}
	}

	// Coverage finds rules that never fire against a recorded corpus.
	corpus := []*ladder.Request{
		{Kind: "worker.idle", Payload: "jv-t1"},
		{Kind: "worker.idle", Payload: "other"},
	}
	matched, dead := l.Coverage(corpus)
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	if len(dead) != 1 || dead[0] != "finished" {
		t.Errorf("dead = %v, want [finished]", dead)
	}
}

func TestRegexIsTheEscapeHatchAndIsBounded(t *testing.T) {
	f := newFixture()
	l := f.patternLayer(t, []ladder.RuleDef{{
		ID: "numbered-worker", Description: "d",
		When:   []ladder.Predicate{{Field: "agent", Kind: ladder.Matches, Value: `^jv-t\d+$`}},
		Answer: "matched",
	}})

	for payload, want := range map[string]bool{"jv-t42": true, "jv-tabc": false, "xjv-t42": false} {
		v, err := l.Evaluate(context.Background(), &ladder.Request{Kind: "k", Payload: payload}, l.Scope().NewReader())
		if err != nil {
			t.Fatal(err)
		}
		got := v.Kind == ladder.VerdictAnswer
		if got != want {
			t.Errorf("%q matched = %v, want %v", payload, got, want)
		}
	}
}

// The substitutability claim, tested rather than asserted: two engines
// with almost nothing in common face the same suite.
func TestBothEnginesSatisfyOneContract(t *testing.T) {
	f := newFixture()
	cases := []laddertest.Case{
		{Name: "matches", Request: &ladder.Request{Kind: "worker.finished", Payload: "jv-t1"}},
		{Name: "abstains", Request: &ladder.Request{Kind: "something.else", Payload: "jv-t1"}},
		{Name: "no payload", Request: &ladder.Request{Kind: "worker.finished"}},
	}

	t.Run("pattern engine", func(t *testing.T) {
		laddertest.Conform(t, &laddertest.Config{
			Layer: f.patternLayer(t, []ladder.RuleDef{{
				ID:          "reap-finished",
				Description: "retire a finished worker",
				When:        []ladder.Predicate{{Field: "kind", Kind: ladder.Equals, Value: "worker.finished"}},
				Action:      "agent.reap",
			}}),
			Cases: cases,
		})
	})

	t.Run("closure engine", func(t *testing.T) {
		s := f.scope(t, "closure", []ladder.Read{f.status}, []ladder.Action{f.reap})
		laddertest.Conform(t, &laddertest.Config{
			Layer: ladder.NewLayerFunc(s, func(ctx context.Context, req *ladder.Request, rd *ladder.Reader) (*ladder.Verdict, error) {
				if _, err := rd.Do(ctx, f.status, req.Payload); err != nil {
					return nil, err
				}
				if req.Kind == "worker.finished" {
					return &ladder.Verdict{Kind: ladder.VerdictAct, Rule: "hand-written", Action: f.reap}, nil
				}
				return &ladder.Verdict{Kind: ladder.VerdictAbstain, Reason: "not mine"}, nil
			}),
			Cases: cases,
		})
	})
}

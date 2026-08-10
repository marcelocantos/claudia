// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

func (f *fixture) scope(t *testing.T, name string, reads []ladder.Read, actions []ladder.Action) *ladder.Scope {
	t.Helper()
	s, err := f.reg.Resolve(&ladder.Manifest{Layer: name, Reads: reads, Actions: actions})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", name, err)
	}
	return s
}

// abstainer is a rung that never answers.
func abstainer(s *ladder.Scope) ladder.Layer {
	return ladder.NewLayerFunc(s, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
		return &ladder.Verdict{Kind: ladder.VerdictAbstain, Reason: "no rule matched"}, nil
	})
}

func TestWalkStopsAtTheFirstAnswerAndRecordsEveryRung(t *testing.T) {
	f := newFixture()
	cheap := f.scope(t, "cheap", nil, nil)
	middle := f.scope(t, "middle", []ladder.Read{f.status}, nil)
	top := f.scope(t, "top", nil, []ladder.Action{f.reap})

	l := ladder.New(
		abstainer(cheap),
		// The middle rung contributes without answering: it resolves
		// something, notes it, and escalates carrying what it added.
		ladder.NewLayerFunc(middle, func(ctx context.Context, req *ladder.Request, rd *ladder.Reader) (*ladder.Verdict, error) {
			phase, err := rd.Do(ctx, f.status, req.Payload)
			if err != nil {
				return nil, err
			}
			req.Note(ladder.Note{Kind: "resolution", Text: "phase", Value: phase})
			return &ladder.Verdict{Kind: ladder.VerdictAbstain, Reason: "resolved but did not decide"}, nil
		}),
		ladder.NewLayerFunc(top, func(ctx context.Context, req *ladder.Request, rd *ladder.Reader) (*ladder.Verdict, error) {
			return &ladder.Verdict{Kind: ladder.VerdictAct, Rule: "reap-idle", Action: f.reap}, nil
		}),
	)

	req := &ladder.Request{Kind: "worker.idle", Payload: "jevons-po"}
	res, err := l.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if res.Layer != "top" || res.Output != "reaped" {
		t.Errorf("answered by %q with output %v, want top/reaped", res.Layer, res.Output)
	}
	if f.reaped != 1 {
		t.Errorf("reaped = %d, want 1", f.reaped)
	}
	if len(res.Rungs) != 3 {
		t.Fatalf("recorded %d rungs, want 3 — every rung a request touches is an artifact", len(res.Rungs))
	}
	if !res.Escalated() {
		t.Error("Escalated() = false after two abstentions")
	}

	// The middle rung's contribution survived to the answering rung,
	// and its read is on its own rung's record.
	if len(req.Notes) != 1 || req.Notes[0].Value != "idle" {
		t.Errorf("notes = %+v, want the resolved phase", req.Notes)
	}
	if got := res.Rungs[1].Reads; len(got) != 1 || got[0].Name != "agent.status" {
		t.Errorf("middle rung reads = %+v", got)
	}
	if len(res.Rungs[0].Reads) != 0 {
		t.Error("a rung that read nothing recorded reads")
	}
}

func TestLayerFailureEscalatesRatherThanDroppingTheRequest(t *testing.T) {
	f := newFixture()
	broken := f.scope(t, "broken", nil, nil)
	panicky := f.scope(t, "panicky", nil, nil)
	top := f.scope(t, "top", nil, []ladder.Action{f.reap})

	l := ladder.New(
		ladder.NewLayerFunc(broken, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
			return nil, errors.New("policy store corrupt")
		}),
		// A layer that returns neither is a contract violation, and the
		// ladder reports it as a defect rather than smoothing over it.
		ladder.NewLayerFunc(panicky, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
			return nil, nil
		}),
		ladder.NewLayerFunc(top, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
			return &ladder.Verdict{Kind: ladder.VerdictAnswer, Answer: "handled"}, nil
		}),
	)

	res, err := l.Evaluate(context.Background(), &ladder.Request{Kind: "x"})
	if err != nil {
		t.Fatalf("a failing rung took the request down instead of escalating: %v", err)
	}
	if res.Layer != "top" || res.Verdict.Answer != "handled" {
		t.Errorf("answered by %q with %v", res.Layer, res.Verdict.Answer)
	}
	if res.Rungs[0].Err == nil {
		t.Error("the failing rung recorded no error")
	}
	if res.Rungs[1].Defect == "" {
		t.Error("returning neither verdict nor error was not recorded as a defect")
	}
}

func TestVerdictOutsideScopeDoesNotAnswerAndDoesNotAct(t *testing.T) {
	f := newFixture()
	overreach := f.scope(t, "overreach", nil, []ladder.Action{f.reap})
	top := f.scope(t, "top", nil, nil)

	l := ladder.New(
		// This rung has a spawn handle in hand but no spawn in its
		// manifest. Capability to name is not licence to act.
		ladder.NewLayerFunc(overreach, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
			return &ladder.Verdict{Kind: ladder.VerdictAct, Rule: "overreach", Action: f.spawn}, nil
		}),
		ladder.NewLayerFunc(top, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
			return &ladder.Verdict{Kind: ladder.VerdictAnswer, Answer: "model handled it"}, nil
		}),
	)

	res, err := l.Evaluate(context.Background(), &ladder.Request{Kind: "x"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if f.spawned != 0 {
		t.Errorf("out-of-scope action executed %d times", f.spawned)
	}
	if res.Layer != "top" {
		t.Errorf("answered by %q; the overreaching rung should have been passed", res.Layer)
	}
	var scopeErr *ladder.ScopeError
	if !errors.As(res.Rungs[0].Err, &scopeErr) {
		t.Errorf("rung 0 error = %v, want a ScopeError naming the drift", res.Rungs[0].Err)
	}
}

func TestAllAbstainingIsAFaultNotAnAnswer(t *testing.T) {
	f := newFixture()
	l := ladder.New(abstainer(f.scope(t, "only", nil, nil)))

	res, err := l.Evaluate(context.Background(), &ladder.Request{Kind: "x"})
	if !errors.Is(err, ladder.ErrNoLayerAnswered) {
		t.Fatalf("err = %v, want ErrNoLayerAnswered — a ladder whose top rung can abstain is missing its top rung", err)
	}
	if len(res.Rungs) != 1 {
		t.Errorf("rungs = %d, want 1", len(res.Rungs))
	}

	// An empty ladder is valid and answers nothing: that is how a
	// consumer that registered no layers keeps its prior behaviour.
	if _, err := ladder.New().Evaluate(context.Background(), &ladder.Request{Kind: "x"}); !errors.Is(err, ladder.ErrNoLayerAnswered) {
		t.Errorf("empty ladder err = %v", err)
	}
}

func TestAnsweringLayerIsStampedWithItsProvenance(t *testing.T) {
	f := newFixture()
	s := f.scope(t, "stamped", nil, nil)
	l := ladder.New(ladder.NewLayerFunc(s, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
		// Deliberately leaves Layer empty; the ladder fills it in, so a
		// consumer can always tell a rule's answer from a model's.
		return &ladder.Verdict{Kind: ladder.VerdictAnswer, Rule: "r1", Reason: "matched", Answer: 42}, nil
	}))

	res, err := l.Evaluate(context.Background(), &ladder.Request{Kind: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict.Layer != "stamped" {
		t.Errorf("verdict layer = %q, want stamped", res.Verdict.Layer)
	}
	if got := res.Verdict.Explain(s); !strings.Contains(got, "stamped (r1)") {
		t.Errorf("Explain() = %q", got)
	}
	if res.Escalated() {
		t.Error("Escalated() = true for an answer at the first rung")
	}
}

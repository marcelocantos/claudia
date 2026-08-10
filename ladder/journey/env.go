// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package journey holds claudia's longitudinal tests for the tiered
// runtime.
//
// A journey drives the real runtime across a SEQUENCE of requests and
// asserts on how behaviour CHANGED across that sequence. A test that
// asserts one call's outcome is not a journey, whatever it is named.
//
// The suite exists because the architecture's central failure is
// invisible in a single shot. When a rung wrongly handles something it
// should have escalated, nothing errors at that instant, no deliberation
// exists to inspect, and the optimised metric IMPROVES. There is no
// moment at which a point-in-time assertion could catch it. It becomes
// observable only as two series diverging across a run sequence — cost
// falling while delivered work falls with it — so a suite made entirely
// of single-shot tests would run green straight through the failure the
// whole design exists to prevent.
//
// Sequence position is the clock. Thresholds counted in runs advance by
// running, so a fifty-run promotion gate costs milliseconds rather than
// an overnight job, and nothing here waits on wall time.
//
// # The live gap, stated rather than left implicit
//
// Every journey here runs against a DETERMINISTIC SCRIPTED provider.
// That is what makes fifty-run sequences affordable, and it is also a
// world in which every decision is perfectly consistent — which is
// precisely the condition under which promotion gating looks flawless
// and proves nothing. These journeys are a regression net. They are not
// evidence that the ladder works.
//
// Two journeys named in 🎯T27.11 are therefore NOT yet covered here, and
// the suite passing does not mean they are:
//
//   - A live-provider journey. Required by the target; not written,
//     because it needs a provider rung wired to a real backend
//     (🎯T27.3/🎯T27.4).
//   - Recorded CONSUMER sessions. The corpora below are synthetic. The
//     root target's oracle asks for real recorded sessions replayed
//     through the ladder, and no amount of scripted-provider work
//     substitutes for that; it has to come from a consumer.
package journey

import (
	"context"
	"fmt"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// Env is one journey's isolated world: its own registry, its own store,
// its own corpus. Nothing is shared between journeys, because a journey
// that can see another's store is measuring something it did not set up.
type Env struct {
	t     *testing.T
	Reg   *ladder.Registry
	Store *ladder.Store

	Reap   ladder.Action
	Status ladder.Read

	// Reaped counts effects actually performed, so a journey can tell a
	// verdict that named an action from one that ran it.
	Reaped int

	// ModelCalls counts turns the scripted model rung was asked for.
	// This is the cost series.
	ModelCalls int

	// modelDown simulates the upper rung being unreachable mid-sequence.
	modelDown bool
	// shapeChanged simulates the world moving under a promoted rule.
	shapeChanged bool
}

// NewEnv builds an isolated environment.
func NewEnv(t *testing.T) *Env {
	t.Helper()
	store, err := ladder.NewStore(nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	e := &Env{t: t, Reg: ladder.NewRegistry(), Store: store}

	e.Reap = e.Reg.Action(&ladder.ActionDef{
		Name:        "agent.reap",
		Description: "retire a worker that has finished and reported",
		Handler: func(context.Context, any) (any, error) {
			e.Reaped++
			return "reaped", nil
		},
	})
	e.Status = e.Reg.Read(&ladder.ReadDef{
		Name:        "agent.status",
		Description: "current phase of a named agent",
		Handler:     func(context.Context, any) (any, error) { return "idle", nil },
	})
	return e
}

// Scope resolves a manifest for one rung.
func (e *Env) Scope(layer string, reads []ladder.Read, actions []ladder.Action) *ladder.Scope {
	e.t.Helper()
	s, err := e.Reg.Resolve(&ladder.Manifest{Layer: layer, Reads: reads, Actions: actions})
	if err != nil {
		e.t.Fatalf("Resolve(%s): %v", layer, err)
	}
	return s
}

// Fields are the request fields rules may match.
func Fields() map[string]ladder.FieldFunc {
	return map[string]ladder.FieldFunc{
		"kind": func(r *ladder.Request) (string, bool) { return r.Kind, r.Kind != "" },
		"agent": func(r *ladder.Request) (string, bool) {
			s, ok := r.Payload.(string)
			return s, ok
		},
	}
}

// ModelRung is the scripted provider: deterministic, and counting every
// turn it is asked for.
//
// A deterministic script is what makes fifty-run sequences affordable.
// It is also a world in which every decision is perfectly consistent,
// which is exactly the condition under which promotion gating looks
// flawless and proves nothing — so it is a regression net, never the
// evidence that the ladder works. See the package-level note on the live
// gap.
func (e *Env) ModelRung() ladder.Layer {
	s := e.Scope("model", nil, []ladder.Action{e.Reap})
	return ladder.NewLayerFunc(s, func(ctx context.Context, req *ladder.Request, rd *ladder.Reader) (*ladder.Verdict, error) {
		if e.modelDown {
			return nil, fmt.Errorf("provider unreachable")
		}
		e.ModelCalls++
		if req.Kind == "worker.finished" {
			return &ladder.Verdict{Kind: ladder.VerdictAct, Rule: "model-judgement", Action: e.Reap}, nil
		}
		return &ladder.Verdict{Kind: ladder.VerdictAnswer, Rule: "model-judgement", Answer: "escalated to the owner"}, nil
	})
}

// SetModelDown makes the upper rung unreachable, so a journey can watch
// what an abstention does when there is nothing above it.
func (e *Env) SetModelDown(down bool) { e.modelDown = down }

// ChangeWorldShape renames the routine event, simulating the production
// case that motivates the circuit breaker: a firmware update changes a
// command's output format and the deterministic parser stops matching.
func (e *Env) ChangeWorldShape(changed bool) { e.shapeChanged = changed }

// Corpus is the recorded request mix: two thirds routine, one third
// genuinely needing judgement.
func (e *Env) Corpus() []*ladder.Request {
	kind := "worker.finished"
	if e.shapeChanged {
		kind = "worker.completed"
	}
	var reqs []*ladder.Request
	for range 20 {
		reqs = append(reqs,
			&ladder.Request{Kind: kind, Payload: "jv-routine"},
			&ladder.Request{Kind: kind, Payload: "jv-routine"},
			&ladder.Request{Kind: "worker.blocked", Payload: "jv-hard"},
		)
	}
	return reqs
}

// Delivered is the consumer's judgement of whether a request produced
// its work. The blocked case is only handled properly when a model saw
// it; everything else is fine either way.
//
// Every cost-claiming journey measures this alongside cost. A journey
// showing the model share falling without also showing outcomes holding
// has measured the wrong thing, and is exactly the measurement a system
// optimising away its own oversight would pass.
func Delivered(req *ladder.Request, res *ladder.Result) bool {
	if res == nil || res.Verdict == nil {
		return false
	}
	if req.Kind == "worker.blocked" {
		return res.Layer == "model"
	}
	return true
}

// RulesAt returns the rules installed at a store version, so a ladder
// can be rebuilt exactly as it stood then.
func RulesAt(v *ladder.Version) []ladder.RuleDef {
	var rules []ladder.RuleDef
	for _, e := range v.Entries {
		if def, ok := e.Rule.(ladder.RuleDef); ok {
			rules = append(rules, def)
		}
	}
	return rules
}

// LadderAt builds the ladder as it stood at a store version: a pattern
// rung holding that version's rules, below the scripted model rung.
func (e *Env) LadderAt(v *ladder.Version) *ladder.Ladder {
	e.t.Helper()
	rules := RulesAt(v)
	if len(rules) == 0 {
		return ladder.New(e.ModelRung())
	}
	pattern, err := ladder.NewPatternLayer(&ladder.PatternConfig{
		Scope:  e.Scope("pattern", nil, []ladder.Action{e.Reap}),
		Fields: Fields(),
		Rules:  rules,
	})
	if err != nil {
		e.t.Fatalf("NewPatternLayer at v%d: %v", v.N, err)
	}
	return ladder.New(pattern, e.ModelRung())
}

// ReplayAt runs the corpus through the ladder as it stood at a version.
func (e *Env) ReplayAt(v *ladder.Version) *ladder.ReplayReport {
	e.t.Helper()
	rep, err := ladder.Replay(context.Background(), &ladder.ReplayArgs{
		Ladder:      e.LadderAt(v),
		Pinned:      v,
		Corpus:      e.Corpus(),
		ModelLayers: []string{"model"},
		Delivered:   Delivered,
	})
	if err != nil {
		e.t.Fatalf("Replay at v%d: %v", v.N, err)
	}
	return rep
}

// Observe runs the sequence until a decision class has been seen `want`
// times, and returns the evidence that accumulated.
//
// SEQUENCE POSITION IS THE CLOCK. A fifty-run promotion gate is reached
// by running fifty times, not by waiting or by asserting that fifty
// happened. One pass over the corpus yields forty routine requests, so
// reaching the deterministic gate genuinely takes more than one pass —
// which is the point: the gate is a property of the sequence, and a
// journey that could satisfy it in a single shot would not be testing it.
func (e *Env) Observe(l *ladder.Ladder, class string, want int) ladder.Evidence {
	e.t.Helper()
	ev := ladder.Evidence{TestsPass: true}
	for pass := 0; ev.Runs < want; pass++ {
		if pass > 100 {
			e.t.Fatalf("observed %d runs of %q after %d passes; the class never accumulates", ev.Runs, class, pass)
		}
		for _, req := range e.Corpus() {
			if req.Kind != class {
				continue
			}
			walked := &ladder.Request{Kind: req.Kind, Payload: req.Payload}
			res, err := l.Evaluate(context.Background(), walked)
			if err != nil || res.Verdict == nil {
				continue
			}
			ev.Runs++
			// The scripted model is deterministic, so every run agrees.
			// That is also why a scripted provider can never be the
			// evidence that promotion gating works.
			ev.Identical++
		}
	}
	return ev
}

// Install runs a full consolidation: propose in one pass, install in the
// next. The two-pass shape is not ceremony — the runtime refuses an
// install in the pass that raised the proposal.
func (e *Env) Install(ruleID string, rule ladder.RuleDef, ev ladder.Evidence, stage ladder.Stage) *ladder.Version {
	e.t.Helper()
	if err := e.Store.Propose(&ladder.Proposal{
		ID:          ruleID,
		Class:       "worker.finished",
		Description: rule.Description,
		Rule:        rule,
		ProposedBy:  "working-pass",
		Pass:        "work",
		Evidence:    ev,
	}); err != nil {
		e.t.Fatalf("Propose(%s): %v", ruleID, err)
	}
	v, err := e.Store.Install(&ladder.InstallArgs{
		ProposalID: ruleID, Installer: "consolidation-pass", Pass: "consolidate", Stage: stage,
	})
	if err != nil {
		e.t.Fatalf("Install(%s): %v", ruleID, err)
	}
	return v
}

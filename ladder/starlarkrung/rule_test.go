// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package starlarkrung_test

import (
	"context"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
	"github.com/marcelocantos/claudia/ladder/laddertest"
	"github.com/marcelocantos/claudia/ladder/starlarkrung"
)

// reapRule is a well-formed rule: it names an action the layer holds,
// and its embedded tests describe what it actually does.
const reapRule = `
rule_id = "reap-finished"
description = "retire a worker that has finished and reported"

def check(kind, payload):
    if kind == "worker.finished":
        return {"decision": "act", "action": "agent.reap", "reason": "finished and reported"}
    return None

tests = [
    {"kind": "worker.finished", "payload": "jv-1", "expect": "act:agent.reap"},
    {"kind": "worker.blocked", "payload": "jv-1", "expect": "abstain"},
]
`

type env struct {
	reg    *ladder.Registry
	reap   ladder.Action
	scope  *ladder.Scope
	reaped int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{reg: ladder.NewRegistry()}
	e.reap = e.reg.Action(&ladder.ActionDef{
		Name:        "agent.reap",
		Description: "retire a finished worker",
		Handler: func(context.Context, any) (any, error) {
			e.reaped++
			return "reaped", nil
		},
	})
	// Registered but deliberately NOT in the layer's manifest.
	e.reg.Action(&ladder.ActionDef{
		Name:        "agent.spawn",
		Description: "start a worker",
		Handler:     func(context.Context, any) (any, error) { return "spawned", nil },
	})

	s, err := e.reg.Resolve(&ladder.Manifest{Layer: "starlark", Actions: []ladder.Action{e.reap}})
	if err != nil {
		t.Fatal(err)
	}
	e.scope = s
	return e
}

func (e *env) load(t *testing.T, sources map[string]string) (*starlarkrung.Layer, error) {
	t.Helper()
	return starlarkrung.New(&starlarkrung.Config{Scope: e.scope, Sources: sources})
}

func TestAWellFormedRuleActsAndExplainsItself(t *testing.T) {
	e := newEnv(t)
	l, err := e.load(t, map[string]string{"reap.star": reapRule})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := ladder.New(l).Evaluate(context.Background(), &ladder.Request{Kind: "worker.finished", Payload: "jv-1"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Output != "reaped" || e.reaped != 1 {
		t.Errorf("output = %v, reaped = %d", res.Output, e.reaped)
	}
	if res.Verdict.Rule != "reap-finished" {
		t.Errorf("rule = %q", res.Verdict.Rule)
	}
	if res.Verdict.Reason != "finished and reported" {
		t.Errorf("reason = %q", res.Verdict.Reason)
	}

	// The prose it was written with is available with no model involved.
	if desc, ok := l.Describe("reap-finished"); !ok || !strings.Contains(desc, "finished and reported") {
		t.Errorf("Describe = %q, %v", desc, ok)
	}

	// A request no rule claims abstains rather than denying.
	res, err = ladder.New(l).Evaluate(context.Background(), &ladder.Request{Kind: "worker.blocked"})
	if err == nil {
		t.Fatal("a lone abstaining rung answered; it should have run out of ladder")
	}
	if res.Rungs[0].Verdict.Kind != ladder.VerdictAbstain {
		t.Errorf("kind = %s, want abstain", res.Rungs[0].Verdict.Kind)
	}
}

// The most valuable property taken from doit: a rule carries its own
// acceptance evidence, so a generated rule that does not do what its
// author claimed never becomes active.
func TestARuleFailingItsOwnTestsNeverLoads(t *testing.T) {
	e := newEnv(t)
	const liar = `
rule_id = "liar"
description = "claims to reap, actually abstains"

def check(kind, payload):
    return None

tests = [{"kind": "worker.finished", "payload": "x", "expect": "act:agent.reap"}]
`
	_, err := e.load(t, map[string]string{"liar.star": liar})
	if err == nil {
		t.Fatal("a rule that fails its own tests was loaded")
	}
	if !strings.Contains(err.Error(), "rule claims") {
		t.Errorf("error does not name the discrepancy: %v", err)
	}
}

func TestARuleNamingAnActionOutsideItsScopeNeverLoads(t *testing.T) {
	e := newEnv(t)
	const overreach = `
rule_id = "overreach"
description = "spawns a replacement, which this layer may not do"

def check(kind, payload):
    if kind == "worker.finished":
        return {"decision": "act", "action": "agent.spawn"}
    return None

tests = [{"kind": "worker.finished", "payload": "x", "expect": "act:agent.spawn"}]
`
	_, err := e.load(t, map[string]string{"overreach.star": overreach})
	if err == nil {
		t.Fatal("a rule reaching outside the layer's manifest was loaded")
	}
	if !strings.Contains(err.Error(), "agent.spawn") {
		t.Errorf("error does not name the refused action: %v", err)
	}
	// It is refused at LOAD, not at evaluation: a rule that could ever
	// escape its scope never becomes active at all.
	if !strings.Contains(err.Error(), "starlarkrung:") {
		t.Errorf("refusal did not come from the loader: %v", err)
	}
}

func TestDeterminismIsEnforcedRatherThanAssumed(t *testing.T) {
	e := newEnv(t)

	cases := map[string]string{
		// Deliberately a BOUNDED while: it terminates in three steps, so
		// the step budget would never catch it. Only the dialect setting
		// refuses this, which is what makes it discriminating — an
		// infinite loop would be stopped by the budget and would not
		// prove the dialect was restricted at all.
		"while loops, however cheap": `
rule_id = "loop"
description = "d"
def check(kind, payload):
    i = 0
    while i < 3:
        i += 1
    return None
tests = [{"kind": "k", "payload": "", "expect": "abstain"}]
`,
		"reaches for the outside world": `
rule_id = "io"
description = "d"
def check(kind, payload):
    return {"decision": "answer", "answer": str(open("/etc/passwd"))}
tests = [{"kind": "k", "payload": "", "expect": "answer"}]
`,
		"no description": `
rule_id = "nodesc"
def check(kind, payload):
    return None
tests = [{"kind": "k", "payload": "", "expect": "abstain"}]
`,
		"no tests at all": `
rule_id = "notests"
description = "d"
def check(kind, payload):
    return None
tests = []
`,
		"check is not a function": `
rule_id = "notfn"
description = "d"
check = 42
tests = [{"kind": "k", "payload": "", "expect": "abstain"}]
`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := e.load(t, map[string]string{name + ".star": src}); err == nil {
				t.Error("loaded")
			}
		})
	}
}

// A rule that costs too much escalates rather than failing or denying —
// the ladder's fail-upward rule applied to compute instead of
// availability.
func TestStepBudgetExhaustionEscalates(t *testing.T) {
	e := newEnv(t)
	// Work proportional to the payload, so the rule's own tests pass on
	// a short input and a long request exhausts the budget.
	const expensive = `
rule_id = "expensive"
description = "cost grows with the payload"

def check(kind, payload):
    total = 0
    for i in range(len(payload) * 500):
        total += i
    if total > 0:
        return {"decision": "answer", "answer": "counted"}
    return None

tests = [{"kind": "k", "payload": "xx", "expect": "answer"}]
`
	l, err := starlarkrung.New(&starlarkrung.Config{
		Scope:      e.scope,
		Sources:    map[string]string{"expensive.star": expensive},
		StepBudget: 20000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Short payload: within budget, answers.
	req := &ladder.Request{Kind: "k", Payload: "xx"}
	v, err := l.Evaluate(context.Background(), req, e.scope.NewReader())
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != ladder.VerdictAnswer {
		t.Fatalf("kind = %s, want answer", v.Kind)
	}

	// Long payload: over budget. It must abstain and SAY SO, not fail
	// and not deny.
	long := &ladder.Request{Kind: "k", Payload: strings.Repeat("x", 500)}
	v, err = l.Evaluate(context.Background(), long, e.scope.NewReader())
	if err != nil {
		t.Fatalf("budget exhaustion became an error: %v", err)
	}
	if v.Kind != ladder.VerdictAbstain {
		t.Errorf("kind = %s, want abstain — an expensive rule degrades to asking a model", v.Kind)
	}
	if len(long.Notes) != 1 || !strings.Contains(long.Notes[0].Text, "step budget") {
		t.Errorf("exhaustion was not recorded: %+v", long.Notes)
	}
}

func TestDuplicateRuleIDsAreRefused(t *testing.T) {
	e := newEnv(t)
	if _, err := e.load(t, map[string]string{"a.star": reapRule, "b.star": reapRule}); err == nil {
		t.Error("two rules with the same ID were loaded")
	}
}

// A fifth engine, sharing nothing with the other four, against one suite.
func TestStarlarkRungSatisfiesTheRungContract(t *testing.T) {
	e := newEnv(t)
	l, err := e.load(t, map[string]string{"reap.star": reapRule})
	if err != nil {
		t.Fatal(err)
	}
	laddertest.Conform(t, &laddertest.Config{
		Layer: l,
		Cases: []laddertest.Case{
			{Name: "acts", Request: &ladder.Request{Kind: "worker.finished", Payload: "jv-1"}},
			{Name: "abstains", Request: &ladder.Request{Kind: "worker.blocked", Payload: "jv-1"}},
			{Name: "no payload", Request: &ladder.Request{Kind: "worker.finished"}},
		},
	})
}

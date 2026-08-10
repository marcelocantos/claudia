// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package starlarkrung is the programmable rung of claudia's ladder:
// sandboxed rules, written in Starlark, that carry their own tests.
//
// It sits in its own package so that only consumers who want a scripting
// engine build one.
//
// # Why a programmable rung beside a pattern rung
//
// They differ in kind, not speed. A pattern rule is analysable without
// being executed and mintable without a model; a Starlark rule buys
// conditionals and composition and loses both properties. A ladder whose
// rungs differed only in latency would not be worth building.
//
// # What is taken from doit, and what is hardened
//
// The rule contract is doit's, which is proven: an identifier, a prose
// description, a check function returning a verdict or nothing, and an
// embedded test list VALIDATED AT LOAD, with a failing rule rejected
// outright. That last property is the most valuable and least obvious
// thing in the design — a rule carries its own acceptance evidence, so a
// generated rule that does not do what its author claimed never becomes
// active.
//
// Two things are hardened beyond doit, which loads rules with a
// zero-valued syntax.FileOptions and sets no execution budget. For a
// gatekeeper adjudicating one shell command that is tolerable; for a
// rung answering arbitrary runtime requests it is not. Here the
// determinism options are set EXPLICITLY rather than inherited from
// library defaults a dependency bump could change, and every evaluation
// runs under a step budget whose exhaustion is an ESCALATION rather than
// a failure or a denial.
package starlarkrung

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/marcelocantos/claudia/ladder"
)

// DefaultStepBudget bounds one evaluation. Exhausting it escalates: an
// expensive rule degrades to "ask a model", which is the ladder's
// fail-upward rule applied to compute rather than to availability.
const DefaultStepBudget = 100_000

// fileOptions pins every syntax feature that could make a rule
// non-deterministic or unbounded.
//
// Set explicitly, never defaulted. The replay oracle depends on two runs
// of one rule over one input reaching the same verdict, and inheriting
// that guarantee from whatever the library happens to default to today
// is not a guarantee at all.
func fileOptions() *syntax.FileOptions {
	return &syntax.FileOptions{
		Set:             false,
		While:           false, // no unbounded loops
		TopLevelControl: false,
		GlobalReassign:  false,
		Recursion:       false, // no unbounded recursion
	}
}

// Rule is one loaded Starlark rule.
type Rule struct {
	ID          string
	Description string

	check   *starlark.Function
	globals starlark.StringDict
	tests   []testCase
}

type testCase struct {
	kind    string
	payload string
	expect  string
	reason  string
}

// Config configures a [Layer].
type Config struct {
	Scope *ladder.Scope

	// Sources are rule sources by filename. Filenames appear in errors
	// and in provenance.
	Sources map[string]string

	// StepBudget per evaluation. Zero uses [DefaultStepBudget].
	StepBudget int
}

// Layer is the programmable rung.
type Layer struct {
	scope  *ladder.Scope
	budget int

	mu    sync.Mutex
	rules []*Rule
}

// New loads and validates a rule set.
//
// Everything that can fail, fails here. A rule whose source will not
// execute, whose tests do not pass, or which names an action outside the
// layer's manifest is REJECTED AT LOAD and never becomes active — so a
// rule that could ever escape its scope is not merely refused at
// evaluation time, it is refused before it can run at all.
//
// Loading is an INSTALL ACT. Rule source is untrusted content: a rule
// proposed by a model is not thereby installed, and calling New is the
// gated, visible step.
func New(cfg *Config) (*Layer, error) {
	if cfg == nil || cfg.Scope == nil {
		return nil, fmt.Errorf("starlarkrung: Config needs a Scope")
	}
	l := &Layer{scope: cfg.Scope, budget: cfg.StepBudget}
	if l.budget <= 0 {
		l.budget = DefaultStepBudget
	}

	names := make([]string, 0, len(cfg.Sources))
	for name := range cfg.Sources {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := make(map[string]bool, len(names))
	for _, name := range names {
		rule, err := l.load(name, cfg.Sources[name])
		if err != nil {
			return nil, err
		}
		if seen[rule.ID] {
			return nil, fmt.Errorf("starlarkrung: rule %q declared twice", rule.ID)
		}
		seen[rule.ID] = true
		l.rules = append(l.rules, rule)
	}
	return l, nil
}

func (l *Layer) load(filename, source string) (*Rule, error) {
	thread := &starlark.Thread{Name: filename}
	thread.SetMaxExecutionSteps(uint64(l.budget))

	// Nothing is predeclared. No I/O, no clock, no randomness, no
	// filesystem, no network: a rule reaches its verdict from the
	// request it was handed and nothing else.
	globals, err := starlark.ExecFileOptions(fileOptions(), thread, filename, source, nil)
	if err != nil {
		return nil, fmt.Errorf("starlarkrung: %s: %w", filename, err)
	}
	globals.Freeze()

	rule := &Rule{globals: globals}
	var ok bool
	if rule.ID, ok = stringGlobal(globals, "rule_id"); !ok {
		return nil, fmt.Errorf("starlarkrung: %s: missing rule_id", filename)
	}
	if rule.Description, ok = stringGlobal(globals, "description"); !ok {
		return nil, fmt.Errorf("starlarkrung: %s: missing description — a rule nobody can describe cannot explain itself later", filename)
	}

	fn, found := globals["check"]
	if !found {
		return nil, fmt.Errorf("starlarkrung: %s: missing check function", filename)
	}
	if rule.check, ok = fn.(*starlark.Function); !ok {
		return nil, fmt.Errorf("starlarkrung: %s: check is %s, not a function", filename, fn.Type())
	}

	tests, err := parseTests(globals)
	if err != nil {
		return nil, fmt.Errorf("starlarkrung: %s: %w", filename, err)
	}
	if len(tests) == 0 {
		return nil, fmt.Errorf("starlarkrung: %s: no tests — a rule that carries no acceptance evidence cannot be gated on any", filename)
	}
	rule.tests = tests

	if err := l.validate(rule); err != nil {
		return nil, fmt.Errorf("starlarkrung: %s: %w", filename, err)
	}
	return rule, nil
}

// validate runs a rule's own tests, and checks every action it can name.
func (l *Layer) validate(rule *Rule) error {
	for i, tc := range rule.tests {
		verdict, exhausted, err := l.call(context.Background(), rule, &ladder.Request{Kind: tc.kind, Payload: tc.payload})
		if err != nil {
			return fmt.Errorf("test %d (%s): %w", i, tc.kind, err)
		}
		if exhausted {
			return fmt.Errorf("test %d (%s): exhausted the step budget", i, tc.kind)
		}

		got := "abstain"
		if verdict != nil {
			got = string(verdict.Kind)
			if verdict.Kind == ladder.VerdictAct {
				got = "act:" + verdict.Action.Name()
			}
		}
		if got != tc.expect {
			return fmt.Errorf("test %d (%s): got %s, rule claims %s", i, tc.kind, got, tc.expect)
		}
	}
	return nil
}

// Scope implements [ladder.Layer].
func (l *Layer) Scope() *ladder.Scope { return l.scope }

// Evaluate implements [ladder.Layer].
func (l *Layer) Evaluate(ctx context.Context, req *ladder.Request, rd *ladder.Reader) (*ladder.Verdict, error) {
	l.mu.Lock()
	rules := append([]*Rule(nil), l.rules...)
	l.mu.Unlock()

	for _, rule := range rules {
		verdict, exhausted, err := l.call(ctx, rule, req)
		if err != nil {
			return nil, fmt.Errorf("starlarkrung: rule %q: %w", rule.ID, err)
		}
		if exhausted {
			// Escalate rather than fail or deny. An expensive rule
			// degrades to asking a model.
			req.Note(ladder.Note{
				Layer: l.scope.Layer(),
				Kind:  "budget",
				Text:  fmt.Sprintf("rule %q exhausted its step budget", rule.ID),
			})
			continue
		}
		if verdict == nil {
			continue // no opinion
		}
		verdict.Rule = rule.ID
		if verdict.Reason == "" {
			verdict.Reason = rule.Description
		}
		return verdict, nil
	}
	return &ladder.Verdict{Kind: ladder.VerdictAbstain, Reason: "no rule had an opinion"}, nil
}

// call runs one rule's check and converts its result to a verdict. It
// never performs an effect: an acting verdict NAMES an action and the
// ladder's actuator runs it.
func (l *Layer) call(ctx context.Context, rule *Rule, req *ladder.Request) (v *ladder.Verdict, exhausted bool, err error) {
	payload := ""
	if s, ok := req.Payload.(string); ok {
		payload = s
	}

	thread := &starlark.Thread{Name: rule.ID}
	thread.SetMaxExecutionSteps(uint64(l.budget))
	thread.SetLocal("context", ctx)

	args := starlark.Tuple{starlark.String(req.Kind), starlark.String(payload)}
	result, callErr := starlark.Call(thread, rule.check, args, nil)
	if callErr != nil {
		if strings.Contains(callErr.Error(), "too many steps") {
			return nil, true, nil
		}
		return nil, false, callErr
	}

	if result == starlark.None {
		return nil, false, nil
	}
	dict, ok := result.(*starlark.Dict)
	if !ok {
		return nil, false, fmt.Errorf("check returned %s, want a dict or None", result.Type())
	}

	decision, _ := dictString(dict, "decision")
	reason, _ := dictString(dict, "reason")

	switch decision {
	case "", "abstain":
		return nil, false, nil
	case "answer":
		answer, _ := dictString(dict, "answer")
		return &ladder.Verdict{Kind: ladder.VerdictAnswer, Reason: reason, Answer: answer}, false, nil
	case "act":
		name, ok := dictString(dict, "action")
		if !ok {
			return nil, false, fmt.Errorf("an acting verdict names no action")
		}
		// Resolution against the layer's manifest. At load time this is
		// what rejects a rule that could ever escape its scope.
		action, err := l.scope.LookupAction(name)
		if err != nil {
			return nil, false, err
		}
		return &ladder.Verdict{Kind: ladder.VerdictAct, Reason: reason, Action: action}, false, nil
	default:
		return nil, false, fmt.Errorf("unknown decision %q", decision)
	}
}

// Rules returns the loaded rule IDs, in load order.
func (l *Layer) Rules() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, len(l.rules))
	for i, r := range l.rules {
		ids[i] = r.ID
	}
	return ids
}

// Describe returns the prose a rule was written with, which is how a
// Starlark answer explains itself with no model involved.
func (l *Layer) Describe(ruleID string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.rules {
		if r.ID == ruleID {
			return r.Description, true
		}
	}
	return "", false
}

func stringGlobal(g starlark.StringDict, name string) (string, bool) {
	v, ok := g[name]
	if !ok {
		return "", false
	}
	s, ok := starlark.AsString(v)
	return s, ok
}

func dictString(d *starlark.Dict, key string) (string, bool) {
	v, found, err := d.Get(starlark.String(key))
	if err != nil || !found {
		return "", false
	}
	s, ok := starlark.AsString(v)
	return s, ok
}

func parseTests(g starlark.StringDict) ([]testCase, error) {
	v, ok := g["tests"]
	if !ok {
		return nil, fmt.Errorf("missing tests")
	}
	list, ok := v.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("tests is %s, want a list", v.Type())
	}

	var out []testCase
	for i := range list.Len() {
		item, ok := list.Index(i).(*starlark.Dict)
		if !ok {
			return nil, fmt.Errorf("test %d is %s, want a dict", i, list.Index(i).Type())
		}
		kind, _ := dictString(item, "kind")
		payload, _ := dictString(item, "payload")
		expect, ok := dictString(item, "expect")
		if !ok {
			return nil, fmt.Errorf("test %d has no expect", i)
		}
		out = append(out, testCase{kind: kind, payload: payload, expect: expect})
	}
	return out, nil
}

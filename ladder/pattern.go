// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// FieldFunc extracts one declared field from a request. A consumer
// declares the fields its rules may match; matching a serialised blob
// instead would let the request's shape change under a rule without any
// rule changing.
type FieldFunc func(req *Request) (value string, present bool)

// PredicateKind is how a predicate compares a field.
type PredicateKind string

const (
	// Equals matches a field exactly.
	Equals PredicateKind = "equals"
	// OneOf matches a field against a set.
	OneOf PredicateKind = "one_of"
	// HasPrefix matches a field's leading characters.
	HasPrefix PredicateKind = "has_prefix"
	// Present matches whenever the field exists at all.
	Present PredicateKind = "present"
	// Matches applies a regular expression.
	//
	// It is the escape hatch, not the default. Go's regexp is RE2:
	// linear time, no backtracking, and no backreferences or lookaround
	// — which is why the cheapest rung in the system cannot become a
	// latency surface. Reaching for Matches where a structured
	// predicate would do is a lint failure, not a harmless choice.
	Matches PredicateKind = "matches"
)

// Predicate is one comparison against one declared field. A rule's
// predicates are conjunctive: all must hold.
type Predicate struct {
	Field  string
	Kind   PredicateKind
	Value  string   // Equals, HasPrefix, Matches
	Values []string // OneOf

	re *regexp.Regexp
}

func (p *Predicate) eval(value string, present bool) bool {
	if !present {
		return false
	}
	switch p.Kind {
	case Equals:
		return value == p.Value
	case OneOf:
		return slices.Contains(p.Values, value)
	case HasPrefix:
		return strings.HasPrefix(value, p.Value)
	case Present:
		return true
	case Matches:
		return p.re.MatchString(value)
	}
	return false
}

// disjointFrom reports whether two predicates on the same field can
// provably never both hold. It is deliberately conservative: anything it
// cannot prove disjoint is reported as possibly overlapping, so the
// overlap analysis never claims separation it has not established.
func (p *Predicate) disjointFrom(q *Predicate) bool {
	switch {
	case p.Kind == Equals && q.Kind == Equals:
		return p.Value != q.Value
	case p.Kind == Equals && q.Kind == OneOf:
		return !slices.Contains(q.Values, p.Value)
	case p.Kind == OneOf && q.Kind == Equals:
		return !slices.Contains(p.Values, q.Value)
	case p.Kind == OneOf && q.Kind == OneOf:
		for _, v := range p.Values {
			if slices.Contains(q.Values, v) {
				return false
			}
		}
		return true
	case p.Kind == Equals && q.Kind == HasPrefix:
		return !strings.HasPrefix(p.Value, q.Value)
	case p.Kind == HasPrefix && q.Kind == Equals:
		return !strings.HasPrefix(q.Value, p.Value)
	case p.Kind == HasPrefix && q.Kind == HasPrefix:
		return !strings.HasPrefix(p.Value, q.Value) && !strings.HasPrefix(q.Value, p.Value)
	}
	// Present matches anything; regex is not analysed structurally.
	return false
}

// RuleDef declares one pattern rule. It is data: a promotion backed by
// observed consistency can emit one by filling a template, with no model
// writing code and no generation step to fail.
type RuleDef struct {
	// ID names the rule in verdicts and in the audit record.
	ID string

	// Description states the situation the rule handles, in prose. It
	// is what the rule's answers explain themselves with.
	Description string

	// When are the conditions, all of which must hold.
	When []Predicate

	// Answer is returned for a rule that decides without an effect.
	Answer any

	// Action names an action by string, resolved against the layer's
	// scope at load time. A rule naming an action the layer may not
	// reach is rejected then, so it never becomes active at all.
	Action string
	Args   any
}

type compiledRule struct {
	def    RuleDef
	action Action
}

// PatternLayer answers by matching declared request fields.
//
// It exists beside a programmable rung because it has three properties
// a programmable rung cannot offer: it is analysable without being
// executed, it is mintable without a model, and it is bounded by
// construction.
type PatternLayer struct {
	scope  *Scope
	fields map[string]FieldFunc
	rules  []compiledRule
}

// PatternConfig configures a [PatternLayer].
type PatternConfig struct {
	Scope *Scope

	// Fields are the request fields rules may match, by name. A rule
	// referring to a field that is not declared here is rejected at
	// load: silently never matching is worse than failing loudly.
	Fields map[string]FieldFunc

	// Rules, evaluated together rather than in order — see
	// [PatternLayer.Evaluate] on why declaration order decides nothing.
	Rules []RuleDef
}

// NewPatternLayer compiles and validates a rule set.
//
// Everything that can fail, fails here: an undeclared field, a pattern
// RE2 will not accept, a duplicate rule ID, or an action outside the
// layer's scope. That last one is the load-time gate — a rule that could
// ever escape its manifest never becomes active.
func NewPatternLayer(cfg *PatternConfig) (*PatternLayer, error) {
	if cfg == nil || cfg.Scope == nil {
		return nil, fmt.Errorf("ladder: PatternConfig needs a Scope")
	}
	l := &PatternLayer{scope: cfg.Scope, fields: cfg.Fields}

	seen := make(map[string]bool, len(cfg.Rules))
	for i := range cfg.Rules {
		def := cfg.Rules[i]
		if def.ID == "" {
			return nil, fmt.Errorf("ladder: rule %d has no ID", i)
		}
		if seen[def.ID] {
			return nil, fmt.Errorf("ladder: rule %q declared twice", def.ID)
		}
		seen[def.ID] = true

		if len(def.When) == 0 {
			return nil, fmt.Errorf("ladder: rule %q has no conditions and would match everything", def.ID)
		}

		when := make([]Predicate, len(def.When))
		copy(when, def.When)
		for j := range when {
			p := &when[j]
			if _, ok := l.fields[p.Field]; !ok {
				return nil, fmt.Errorf("ladder: rule %q matches undeclared field %q", def.ID, p.Field)
			}
			switch p.Kind {
			case Equals, HasPrefix, Present:
			case OneOf:
				if len(p.Values) == 0 {
					return nil, fmt.Errorf("ladder: rule %q has an empty one_of on %q", def.ID, p.Field)
				}
			case Matches:
				re, err := regexp.Compile(p.Value)
				if err != nil {
					return nil, fmt.Errorf("ladder: rule %q pattern on %q: %w", def.ID, p.Field, err)
				}
				p.re = re
			default:
				return nil, fmt.Errorf("ladder: rule %q has unknown predicate kind %q", def.ID, p.Kind)
			}
		}
		def.When = when

		cr := compiledRule{def: def}
		if def.Action != "" {
			a, err := cfg.Scope.LookupAction(def.Action)
			if err != nil {
				return nil, fmt.Errorf("ladder: rule %q: %w", def.ID, err)
			}
			cr.action = a
		}
		l.rules = append(l.rules, cr)
	}
	return l, nil
}

// Scope implements [Layer].
func (l *PatternLayer) Scope() *Scope { return l.scope }

// Evaluate implements [Layer].
//
// All rules are evaluated, not just the first to match. Nothing matching
// is an abstention; exactly one match is that rule's verdict; and two
// rules disagreeing ABSTAINS with the conflict noted, rather than
// resolving it by declaration order. Silent first-match-wins would make
// rule ordering into invisible policy, and the ambiguity is exactly the
// case a cheap rung should decline.
func (l *PatternLayer) Evaluate(ctx context.Context, req *Request, rd *Reader) (*Verdict, error) {
	var matched []compiledRule
	for _, cr := range l.rules {
		if l.matches(&cr, req) {
			matched = append(matched, cr)
		}
	}

	switch len(matched) {
	case 0:
		return &Verdict{Kind: VerdictAbstain, Reason: "no rule matched"}, nil
	case 1:
		return l.verdictFor(matched[0]), nil
	}

	first := l.verdictFor(matched[0])
	for _, cr := range matched[1:] {
		if v := l.verdictFor(cr); !sameOutcome(first, v) {
			ids := make([]string, len(matched))
			for i, m := range matched {
				ids[i] = m.def.ID
			}
			sort.Strings(ids)
			// Escalate, and say so. An ambiguity resolved silently is a
			// decision made invisibly.
			req.Note(Note{
				Layer: l.scope.Layer(),
				Kind:  "ambiguity",
				Text:  "rules disagree: " + strings.Join(ids, ", "),
			})
			return &Verdict{
				Kind:   VerdictAbstain,
				Reason: "conflicting rules matched: " + strings.Join(ids, ", "),
			}, nil
		}
	}
	// Same outcome from several rules collapses to one answer.
	return first, nil
}

func (l *PatternLayer) matches(cr *compiledRule, req *Request) bool {
	for i := range cr.def.When {
		p := &cr.def.When[i]
		value, present := l.fields[p.Field](req)
		if !p.eval(value, present) {
			return false
		}
	}
	return true
}

func (l *PatternLayer) verdictFor(cr compiledRule) *Verdict {
	v := &Verdict{
		Layer:  l.scope.Layer(),
		Rule:   cr.def.ID,
		Reason: cr.def.Description,
	}
	if cr.def.Action != "" {
		v.Kind = VerdictAct
		v.Action = cr.action
		v.Args = cr.def.Args
		return v
	}
	v.Kind = VerdictAnswer
	v.Answer = cr.def.Answer
	return v
}

func sameOutcome(a, b *Verdict) bool {
	return a.Kind == b.Kind && a.Action.Name() == b.Action.Name() && fmt.Sprint(a.Answer) == fmt.Sprint(b.Answer)
}

// Overlap names two rules that can both match the same request.
type Overlap struct{ A, B string }

// Overlaps reports rule pairs that are not provably disjoint, WITHOUT
// executing anything.
//
// This is the property that earns the pattern rung its place beside a
// programmable one: a Starlark predicate can only be reasoned about by
// running it on inputs you already thought of. The analysis is
// conservative — a pair it cannot prove separate is reported — because
// a false claim of separation is the answer that would hurt.
func (l *PatternLayer) Overlaps() []Overlap {
	var out []Overlap
	for i := 0; i < len(l.rules); i++ {
		for j := i + 1; j < len(l.rules); j++ {
			if !provablyDisjoint(l.rules[i].def.When, l.rules[j].def.When) {
				out = append(out, Overlap{A: l.rules[i].def.ID, B: l.rules[j].def.ID})
			}
		}
	}
	return out
}

func provablyDisjoint(a, b []Predicate) bool {
	for i := range a {
		for j := range b {
			if a[i].Field == b[j].Field && a[i].disjointFrom(&b[j]) {
				return true
			}
		}
	}
	return false
}

// Coverage runs a recorded corpus through the rule set and reports which
// rules never fired. A rule that matches nothing is either dead or
// waiting for a case that no longer occurs, and both are worth knowing
// before it is trusted.
func (l *PatternLayer) Coverage(corpus []*Request) (matchedRequests int, deadRules []string) {
	fired := make(map[string]bool, len(l.rules))
	for _, req := range corpus {
		hit := false
		for i := range l.rules {
			if l.matches(&l.rules[i], req) {
				fired[l.rules[i].def.ID] = true
				hit = true
			}
		}
		if hit {
			matchedRequests++
		}
	}
	for i := range l.rules {
		if !fired[l.rules[i].def.ID] {
			deadRules = append(deadRules, l.rules[i].def.ID)
		}
	}
	sort.Strings(deadRules)
	return matchedRequests, deadRules
}

// Rules returns the rule IDs in declaration order, so a consumer can
// enumerate what a layer holds without reaching inside it.
func (l *PatternLayer) Rules() []string {
	ids := make([]string, len(l.rules))
	for i := range l.rules {
		ids[i] = l.rules[i].def.ID
	}
	return ids
}

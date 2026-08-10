// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
)

// Tier is one model-backed rung, described by what it can do and what it
// costs relative to its peers.
//
// A tier names a CAPABILITY and a COST CLASS, never a vendor. Claude,
// Codex, Grok, Bedrock and a local backend are all eligible for any rung
// they can satisfy, and swapping one for another is a configuration
// change rather than a routing change.
type Tier struct {
	// Name identifies the tier in verdicts, evidence and routes.
	Name string

	// Cost orders tiers against each other. Lower is cheaper. The
	// absolute value means nothing; only the ordering is used.
	Cost int

	// Capabilities the tier can satisfy.
	Capabilities []string

	// Layer is the rung itself. It satisfies the same contract as every
	// other rung, so a tier is substitutable for a rule.
	Layer Layer
}

// Boundary is one crossing between two tiers for one decision class.
//
// Evidence is held per boundary and never pooled. A class that a cheap
// model handles as well as an expensive one has earned exactly that
// crossing, and nothing about any other.
type Boundary struct {
	Class string
	From  string
	To    string
}

func (b Boundary) String() string {
	return fmt.Sprintf("%s: %s→%s", b.Class, b.From, b.To)
}

// AgreementLog accumulates per-boundary evidence that a cheaper tier
// decides a class the same way a dearer one does.
type AgreementLog struct {
	mu sync.Mutex
	ev map[Boundary]*Evidence
}

// NewAgreementLog returns an empty log.
func NewAgreementLog() *AgreementLog {
	return &AgreementLog{ev: make(map[Boundary]*Evidence)}
}

// Record notes one observation at a boundary.
func (a *AgreementLog) Record(b Boundary, agreed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	e, ok := a.ev[b]
	if !ok {
		e = &Evidence{TestsPass: true}
		a.ev[b] = e
	}
	e.Runs++
	if agreed {
		e.Identical++
	}
}

// Evidence returns what has been observed at a boundary.
func (a *AgreementLog) Evidence(b Boundary) Evidence {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ev[b]; ok {
		return *e
	}
	return Evidence{TestsPass: true}
}

// Boundaries returns every observed boundary, sorted.
func (a *AgreementLog) Boundaries() []Boundary {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Boundary, 0, len(a.ev))
	for b := range a.ev {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// RouterConfig configures a [Router].
type RouterConfig struct {
	Scope *Scope

	// Tiers, in any order; the router sorts them by cost.
	Tiers []Tier

	// Requires reports the capabilities a request needs. Returning
	// nothing means any tier will do, so the cheapest wins.
	Requires func(req *Request) []string

	// Thresholds gate a route being pinned to a cheaper tier. Only the
	// agent→hybrid pair is consulted: a model-to-model crossing is that
	// transition, not the crossing into a rule.
	Thresholds *Thresholds
}

// Router is the model tier as a ladder of its own.
//
// An escalation from a deterministic rung lands on the CHEAPEST model
// plausibly able to decide, not automatically on the most capable one
// available. A decision the cheap model makes identically to the dear
// one for enough runs can then be pinned to it — a resting place between
// "the frontier model decides" and "a rule decides" that is safer than
// crystallisation, because a cheap model still generalises where a rule
// cannot.
//
// It composes with a broker rather than duplicating one: a router
// SELECTS a tier and says nothing about concurrency, warm pools,
// priority or preemption, which belong to whatever schedules the work.
type Router struct {
	scope      *Scope
	tiers      []Tier
	requires   func(*Request) []string
	thresholds *Thresholds

	mu     sync.RWMutex
	routes map[string]string // class → pinned tier
}

// NewRouter builds a router over the given tiers.
func NewRouter(cfg *RouterConfig) (*Router, error) {
	if cfg == nil || cfg.Scope == nil {
		return nil, fmt.Errorf("ladder: RouterConfig needs a Scope")
	}
	if len(cfg.Tiers) == 0 {
		return nil, fmt.Errorf("ladder: a router needs at least one tier")
	}

	seen := make(map[string]bool, len(cfg.Tiers))
	for _, t := range cfg.Tiers {
		if t.Name == "" {
			return nil, fmt.Errorf("ladder: a tier has no name")
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("ladder: tier %q declared twice", t.Name)
		}
		seen[t.Name] = true
		if t.Layer == nil {
			return nil, fmt.Errorf("ladder: tier %q has no layer", t.Name)
		}
	}

	r := &Router{
		scope:      cfg.Scope,
		tiers:      slices.Clone(cfg.Tiers),
		requires:   cfg.Requires,
		thresholds: cfg.Thresholds,
		routes:     make(map[string]string),
	}
	if r.thresholds == nil {
		r.thresholds = ProductionThresholds()
	}
	sort.SliceStable(r.tiers, func(i, j int) bool { return r.tiers[i].Cost < r.tiers[j].Cost })
	return r, nil
}

// Scope implements [Layer].
func (r *Router) Scope() *Scope { return r.scope }

// Evaluate implements [Layer] by delegating to the selected tier.
func (r *Router) Evaluate(ctx context.Context, req *Request, rd *Reader) (*Verdict, error) {
	tier, err := r.Select(req)
	if err != nil {
		return nil, err
	}
	verdict, err := tier.Layer.Evaluate(ctx, req, rd)
	if err != nil || verdict == nil {
		return verdict, err
	}
	// Stamp the tier that actually answered, so a consumer's cost
	// accounting can tell a cheap model's answer from a dear one's.
	verdict.Layer = tier.Name
	return verdict, nil
}

// Select returns the tier a request routes to: the pinned one if a route
// has been earned for its class, otherwise the cheapest tier whose
// capabilities cover what the request needs.
func (r *Router) Select(req *Request) (Tier, error) {
	need := r.needs(req)

	r.mu.RLock()
	pinned, hasRoute := r.routes[req.Kind]
	r.mu.RUnlock()

	if hasRoute {
		for _, t := range r.tiers {
			if t.Name == pinned {
				return t, nil
			}
		}
	}

	for _, t := range r.tiers {
		if covers(t.Capabilities, need) {
			return t, nil
		}
	}
	return Tier{}, fmt.Errorf("ladder: no tier satisfies %v for request kind %q", need, req.Kind)
}

func (r *Router) needs(req *Request) []string {
	if r.requires == nil {
		return nil
	}
	return r.requires(req)
}

func covers(have, need []string) bool {
	for _, n := range need {
		if !slices.Contains(have, n) {
			return false
		}
	}
	return true
}

// Shadow runs a request on two tiers and records whether they agreed.
//
// This is an OFFLINE comparison, run deliberately to gather evidence,
// never a hot-path double call — paying for both tiers on every request
// would spend more than the routing could ever save.
func (r *Router) Shadow(ctx context.Context, req *Request, log *AgreementLog, from, to string) error {
	fromTier, err := r.tier(from)
	if err != nil {
		return err
	}
	toTier, err := r.tier(to)
	if err != nil {
		return err
	}

	fromVerdict, fromErr := fromTier.Layer.Evaluate(ctx, req, fromTier.Layer.Scope().NewReader())
	toVerdict, toErr := toTier.Layer.Evaluate(ctx, req, toTier.Layer.Scope().NewReader())
	if fromErr != nil || toErr != nil || fromVerdict == nil || toVerdict == nil {
		// A failed comparison is not evidence of agreement OR of
		// disagreement, so it is not recorded at all.
		return nil
	}

	log.Record(Boundary{Class: req.Kind, From: from, To: to}, sameOutcome(fromVerdict, toVerdict))
	return nil
}

// Pin routes a class to a cheaper tier, if that BOUNDARY has earned it.
//
// Evidence is checked for exactly the crossing being made. A class the
// cheap tier has proven itself on does not thereby license routing a
// different class, or routing to a different tier, however good the
// numbers look elsewhere.
func (r *Router) Pin(b Boundary, log *AgreementLog) error {
	from, err := r.tier(b.From)
	if err != nil {
		return err
	}
	to, err := r.tier(b.To)
	if err != nil {
		return err
	}
	if to.Cost >= from.Cost {
		return fmt.Errorf("ladder: %s is not a demotion — %q costs %d, %q costs %d", b, b.To, to.Cost, b.From, from.Cost)
	}

	ev := log.Evidence(b)
	if ev.Runs < r.thresholds.AgentToHybridRuns {
		return fmt.Errorf("%w: %s has %d runs, want %d", ErrInsufficientEvidence, b, ev.Runs, r.thresholds.AgentToHybridRuns)
	}
	if ev.Consistency() < r.thresholds.AgentToHybridConsistency {
		return fmt.Errorf("%w: %s agrees %.2f of the time, want %.2f", ErrInsufficientEvidence, b, ev.Consistency(), r.thresholds.AgentToHybridConsistency)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[b.Class] = b.To
	return nil
}

// Unpin removes a route, sending the class back to ordinary selection.
// Like every demotion it needs no evidence: stopping is always allowed.
func (r *Router) Unpin(class string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, class)
}

// Routes returns the pinned class→tier routes.
func (r *Router) Routes() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return maps.Clone(r.routes)
}

func (r *Router) tier(name string) (Tier, error) {
	for _, t := range r.tiers {
		if t.Name == name {
			return t, nil
		}
	}
	return Tier{}, fmt.Errorf("ladder: no tier %q", name)
}

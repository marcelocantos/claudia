// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package ladder is claudia's tiered-cognition runtime: a request is
// answered by the cheapest layer that can answer it, and a model is the
// escalation path rather than the entry point.
//
// A consumer builds a [Registry] of the actions and reads its layers may
// use, then constructs each layer with a [Manifest] assembled from the
// handles that registration returned. A handle carries an unexported
// name, so only the registry can mint one and a layer cannot be scoped
// to something that was never registered: the mistake is a compile
// error rather than a 3am runtime denial.
//
// Reads and actions are deliberately different things.
//
// A [Read] is side-effect-free and available to a layer's classifier.
// Its result is recorded, so a request replays deterministically without
// the classifier having to abstain from looking anything up.
//
// An [Action] is effectful and is reachable only through
// [Scope.Perform], from a [Verdict] that names it. A layer never
// performs an action; it says which one it wants. Manifest enforcement
// therefore happens before anything executes, an interpreted rule needs
// no effectful injection at all, and replay replays the verdict rather
// than re-running the effect.
//
// Claudia supplies the mechanism and no rules. Every action, read, rule
// and layer is the consumer's.
package ladder

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ActionFunc performs an effectful action on a consumer's behalf. It is
// invoked only by [Scope.Perform], and only for an action the calling
// layer's manifest covers.
type ActionFunc func(ctx context.Context, args any) (any, error)

// ReadFunc answers a side-effect-free query.
//
// A ReadFunc must not mutate observable state. Its result is recorded
// and, on replay, served from the recording without the function being
// called again — so a read with side effects would silently stop
// happening the second time around.
type ReadFunc func(ctx context.Context, args any) (any, error)

// DecodeFunc converts arguments supplied by an untrusted caller — an
// interpreted rule, or a model — into the value the handler expects.
//
// It is shape translation only. A DecodeFunc that defaults a missing
// field, synthesises an argument, or applies policy is a second
// implementation of the action wearing a different hat, and would be
// invisible to any check that compares handlers.
type DecodeFunc func(raw json.RawMessage) (any, error)

// ActionDef declares one effectful action. Name and Handler are
// required. Decode is required only where the action is reachable from
// an interpreted rule or exposed to a model, since both name it by
// string.
type ActionDef struct {
	// Name is how an interpreted rule, a model, or the audit record
	// refers to this action. Go callers use the returned [Action]
	// instead.
	Name string

	// Description states the situation the action is for, in prose. It
	// is what lets a deterministic answer explain itself without a
	// model being involved.
	Description string

	// ModelExposed reports whether this action also appears on the
	// model-facing surface. An action with no model-facing counterpart
	// is a deliberate decision: it gives the least-supervised actor in
	// the system a capability the supervised one never had.
	ModelExposed bool

	// Handler is the single implementation. Both surfaces are thin
	// adapters over it, so the cheap path and the expensive path cannot
	// disagree about what the action means.
	Handler ActionFunc

	// Decode translates string-originated arguments. Nil means the
	// action is reachable only from Go.
	Decode DecodeFunc
}

// ReadDef declares one side-effect-free query. Name and Handler are
// required; Decode is required only where an interpreted rule may name
// the read.
type ReadDef struct {
	Name        string
	Description string
	Handler     ReadFunc
	Decode      DecodeFunc
}

// Action is an unforgeable handle to a registered action. Its zero value
// is invalid and is rejected wherever a handle is accepted.
//
// The unexported fields are the point: a name that was never registered
// cannot be written down, and a handle minted by one registry cannot be
// used against another.
type Action struct {
	name string
	reg  *Registry
}

// Name returns the string an interpreted rule or a model would use to
// name this action. Handle to string always succeeds; string to handle
// is a resolution that can fail (see [Scope.LookupAction]).
func (a Action) Name() string { return a.name }

// IsZero reports whether a is the unusable zero handle.
func (a Action) IsZero() bool { return a.reg == nil }

func (a Action) String() string {
	if a.IsZero() {
		return "ladder.Action(invalid)"
	}
	return a.name
}

// Read is an unforgeable handle to a registered read. Its zero value is
// invalid. See [Action] for why handles rather than strings.
type Read struct {
	name string
	reg  *Registry
}

// Name returns the string an interpreted rule would use to name this
// read.
func (r Read) Name() string { return r.name }

// IsZero reports whether r is the unusable zero handle.
func (r Read) IsZero() bool { return r.reg == nil }

func (r Read) String() string {
	if r.IsZero() {
		return "ladder.Read(invalid)"
	}
	return r.name
}

// Registry holds every action and read a consumer's layers may use. It
// is the single source of truth both surfaces adapt over: one
// implementation per action, whichever surface reached it.
//
// Build one at construction, register everything, and hand the returned
// handles to the layers that need them. Registration is safe for
// concurrent use but is expected to happen at startup.
type Registry struct {
	mu      sync.RWMutex
	actions map[string]*ActionDef
	reads   map[string]*ReadDef
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		actions: make(map[string]*ActionDef),
		reads:   make(map[string]*ReadDef),
	}
}

// Action registers an effectful action and returns its handle.
//
// It panics on a duplicate name, an empty name, or a nil handler. Those
// are static mistakes in a consumer's startup code, and failing at
// construction is the whole point of scoping layers by handle: the
// alternative is discovering the typo when a rule fires at 3am.
func (r *Registry) Action(def *ActionDef) Action {
	if def == nil {
		panic("ladder: nil ActionDef")
	}
	if def.Name == "" {
		panic("ladder: action with empty name")
	}
	if def.Handler == nil {
		panic("ladder: action " + def.Name + " has nil Handler")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actions[def.Name]; dup {
		panic("ladder: action " + def.Name + " registered twice")
	}
	cp := *def
	r.actions[def.Name] = &cp
	return Action{name: def.Name, reg: r}
}

// Read registers a side-effect-free query and returns its handle. It
// panics under the same conditions as [Registry.Action].
func (r *Registry) Read(def *ReadDef) Read {
	if def == nil {
		panic("ladder: nil ReadDef")
	}
	if def.Name == "" {
		panic("ladder: read with empty name")
	}
	if def.Handler == nil {
		panic("ladder: read " + def.Name + " has nil Handler")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.reads[def.Name]; dup {
		panic("ladder: read " + def.Name + " registered twice")
	}
	cp := *def
	r.reads[def.Name] = &cp
	return Read{name: def.Name, reg: r}
}

// ModelActions returns the names of actions exposed on the model-facing
// surface, sorted. The remainder are reachable only by layers, which is
// a declared decision per action rather than an accident of
// registration.
func (r *Registry) ModelActions() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var names []string
	for name, def := range r.actions {
		if def.ModelExposed {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// LayerOnlyActions returns the names of actions that no model can reach,
// sorted. A consumer audits this list deliberately: everything on it is
// a capability the cheapest actor in the system has and the supervised
// one does not.
func (r *Registry) LayerOnlyActions() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var names []string
	for name, def := range r.actions {
		if !def.ModelExposed {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// DescribeAction returns the prose an action was registered with, which
// is how a deterministic answer explains itself with no model involved.
func (r *Registry) DescribeAction(a Action) (string, error) {
	if a.IsZero() {
		return "", fmt.Errorf("ladder: zero Action handle")
	}
	if a.reg != r {
		return "", fmt.Errorf("ladder: action %q belongs to a different registry", a.name)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.actions[a.name]
	if !ok {
		return "", fmt.Errorf("ladder: action %q is not registered", a.name)
	}
	return def.Description, nil
}

func (r *Registry) actionDef(name string) (*ActionDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.actions[name]
	return def, ok
}

func (r *Registry) readDef(name string) (*ReadDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.reads[name]
	return def, ok
}

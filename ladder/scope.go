// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"context"
	"fmt"
	"sync"
)

// Manifest declares what one layer may reach. It is assembled from
// handles, never from strings, so a manifest cannot name something that
// was never registered.
//
// The manifest is the layer's surface rather than a policy consulted at
// call time: what is absent does not exist for that layer. An L1 that
// may reap a finished worker does not thereby may spawn one, and the
// distinction is enforced by construction rather than by an
// argument-level check on a shared schema.
type Manifest struct {
	// Layer names the layer for provenance and for scope violations. It
	// is required: an anonymous layer produces audit records nobody can
	// act on.
	Layer string

	// Reads are the side-effect-free queries the layer's classifier may
	// issue.
	Reads []Read

	// Actions are the effectful actions the layer's verdicts may name.
	Actions []Action
}

// Scope is a resolved [Manifest] — the namespace one layer actually
// has. Resolution happens once, at construction, so a handle that does
// not belong fails there rather than when a rule fires.
type Scope struct {
	reg     *Registry
	layer   string
	reads   map[string]Read
	actions map[string]Action
}

// ScopeError reports an attempt to use something outside a layer's
// manifest.
//
// This is the load-bearing event kind. A denial is loud and
// self-reporting; a layer quietly widening what it handles is not, and
// quiet failure is this architecture's characteristic weakness on both
// the cognition and the authority axis.
type ScopeError struct {
	Layer string
	Kind  string // "action" or "read"
	Name  string
}

func (e *ScopeError) Error() string {
	return fmt.Sprintf("ladder: layer %q may not use %s %q", e.Layer, e.Kind, e.Name)
}

// Resolve turns a manifest into a scope, rejecting anything a layer
// could not legitimately have been given: a zero handle, or a handle
// minted by a different registry.
func (r *Registry) Resolve(m *Manifest) (*Scope, error) {
	if m == nil {
		return nil, fmt.Errorf("ladder: nil Manifest")
	}
	if m.Layer == "" {
		return nil, fmt.Errorf("ladder: Manifest.Layer is required")
	}

	s := &Scope{
		reg:     r,
		layer:   m.Layer,
		reads:   make(map[string]Read, len(m.Reads)),
		actions: make(map[string]Action, len(m.Actions)),
	}

	for _, rd := range m.Reads {
		if rd.IsZero() {
			return nil, fmt.Errorf("ladder: layer %q: zero Read handle", m.Layer)
		}
		if rd.reg != r {
			return nil, fmt.Errorf("ladder: layer %q: read %q belongs to a different registry", m.Layer, rd.name)
		}
		if _, ok := r.readDef(rd.name); !ok {
			return nil, fmt.Errorf("ladder: layer %q: read %q is not registered", m.Layer, rd.name)
		}
		s.reads[rd.name] = rd
	}

	for _, a := range m.Actions {
		if a.IsZero() {
			return nil, fmt.Errorf("ladder: layer %q: zero Action handle", m.Layer)
		}
		if a.reg != r {
			return nil, fmt.Errorf("ladder: layer %q: action %q belongs to a different registry", m.Layer, a.name)
		}
		if _, ok := r.actionDef(a.name); !ok {
			return nil, fmt.Errorf("ladder: layer %q: action %q is not registered", m.Layer, a.name)
		}
		s.actions[a.name] = a
	}

	return s, nil
}

// Layer returns the name this scope was resolved for.
func (s *Scope) Layer() string { return s.layer }

// AllowsAction reports whether a is in scope.
func (s *Scope) AllowsAction(a Action) bool {
	if a.IsZero() || a.reg != s.reg {
		return false
	}
	_, ok := s.actions[a.name]
	return ok
}

// AllowsRead reports whether rd is in scope.
func (s *Scope) AllowsRead(rd Read) bool {
	if rd.IsZero() || rd.reg != s.reg {
		return false
	}
	_, ok := s.reads[rd.name]
	return ok
}

// LookupAction resolves a string to a handle, which is how an untrusted
// caller — an interpreted rule, or a model — names an action.
//
// Resolution runs one way. Handle to string always succeeds; this
// direction can fail, and it fails for anything outside the layer's
// scope. For interpreted rules that means calling this at load time, so
// a rule naming an action it may not reach is rejected before it ever
// becomes active rather than when it first fires.
func (s *Scope) LookupAction(name string) (Action, error) {
	a, ok := s.actions[name]
	if !ok {
		return Action{}, &ScopeError{Layer: s.layer, Kind: "action", Name: name}
	}
	return a, nil
}

// LookupRead resolves a string to a read handle, with the same one-way
// semantics as [Scope.LookupAction].
func (s *Scope) LookupRead(name string) (Read, error) {
	rd, ok := s.reads[name]
	if !ok {
		return Read{}, &ScopeError{Layer: s.layer, Kind: "read", Name: name}
	}
	return rd, nil
}

// Perform executes the action a verdict names.
//
// It is the only path from a verdict to an effect, and it validates
// before it executes: a layer that names an action outside its scope is
// refused with nothing having happened. This is what lets a layer return
// a verdict rather than perform one — the classifier stays pure and
// replayable, and the actuator is the single place authority is checked.
func (s *Scope) Perform(ctx context.Context, v *Verdict) (any, error) {
	if err := v.Validate(s); err != nil {
		return nil, err
	}
	if v.Kind != VerdictAct {
		return nil, fmt.Errorf("ladder: verdict of kind %s names no action to perform", v.Kind)
	}
	def, ok := s.reg.actionDef(v.Action.name)
	if !ok {
		return nil, fmt.Errorf("ladder: action %q is not registered", v.Action.name)
	}
	return def.Handler(ctx, v.Args)
}

// ReadRecord is one read and what it answered. A request's records are
// what make replay deterministic: on replay the recorded answer is
// served and the underlying query is never re-issued.
type ReadRecord struct {
	Name   string `json:"name"`
	Args   any    `json:"args,omitempty"`
	Result any    `json:"result,omitempty"`
	Err    string `json:"err,omitempty"`
}

// Reader issues a layer's reads for one request and records every
// answer.
//
// Purity here means determinism, not abstinence. A classifier may look
// things up; what it may not do is get a different answer on replay, and
// recording is how that is guaranteed without forbidding lookups
// outright.
type Reader struct {
	scope *Scope

	mu       sync.Mutex
	recorded []ReadRecord

	// replaying selects replay mode. It is a flag rather than a nil
	// check on replay, so replaying an empty recording stays a replay:
	// a request that read nothing must not silently become one that
	// reads freely.
	replaying bool
	replay    []ReadRecord
	next      int
}

// NewReader returns a live Reader that issues reads and records them.
func (s *Scope) NewReader() *Reader {
	return &Reader{scope: s}
}

// NewReplayReader returns a Reader that serves recorded answers instead
// of issuing reads. A read that does not match the recording in order
// and name is an error rather than a fresh query: the recording is
// evidence about one request, and quietly filling a gap would make it
// evidence about a different one.
func (s *Scope) NewReplayReader(records []ReadRecord) *Reader {
	return &Reader{scope: s, replaying: true, replay: records}
}

// Do issues a read, or serves it from the recording in replay mode.
func (r *Reader) Do(ctx context.Context, rd Read, args any) (any, error) {
	if !r.scope.AllowsRead(rd) {
		name := rd.name
		if rd.IsZero() {
			name = "(zero handle)"
		}
		return nil, &ScopeError{Layer: r.scope.layer, Kind: "read", Name: name}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.replaying {
		if r.next >= len(r.replay) {
			return nil, fmt.Errorf("ladder: replay exhausted at read %q (recording has %d)", rd.name, len(r.replay))
		}
		rec := r.replay[r.next]
		r.next++
		if rec.Name != rd.name {
			return nil, fmt.Errorf("ladder: replay divergence at position %d: recorded %q, replayed %q", r.next-1, rec.Name, rd.name)
		}
		if rec.Err != "" {
			return nil, fmt.Errorf("%s", rec.Err)
		}
		return rec.Result, nil
	}

	def, ok := r.scope.reg.readDef(rd.name)
	if !ok {
		return nil, fmt.Errorf("ladder: read %q is not registered", rd.name)
	}
	result, err := def.Handler(ctx, args)
	rec := ReadRecord{Name: rd.name, Args: args, Result: result}
	if err != nil {
		rec.Result = nil
		rec.Err = err.Error()
	}
	r.recorded = append(r.recorded, rec)
	return result, err
}

// Records returns the reads issued so far, in order.
func (r *Reader) Records() []ReadRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReadRecord(nil), r.recorded...)
}

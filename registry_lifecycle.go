// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
)

// ErrLifecycleInProgress means a conflicting lifecycle change must be retried.
var ErrLifecycleInProgress = errors.New("agent lifecycle operation in progress")

// Reservations outlive provider work AND cleanup. Stop waiters take priority
// over queued starts, and the entry survives Remove until all waiters leave.
// Every field except gate is protected by Registry.mu.
type registryLifecycle struct {
	gate   chan struct{}
	refs   int
	stops  int
	epoch  uint64
	cancel context.CancelFunc
}

func (r *Registry) beginLifecycle(ctx context.Context, name string, stop bool) (context.Context, *registryLifecycle, func(), error) {
	r.mu.Lock()
	if r.lifecycle == nil {
		r.lifecycle = make(map[string]*registryLifecycle)
	}
	op := r.lifecycle[name]
	if op == nil {
		op = &registryLifecycle{gate: make(chan struct{}, 1)}
		r.lifecycle[name] = op
	}
	if !stop && op.stops > 0 {
		r.mu.Unlock()
		return nil, nil, nil, ErrLifecycleInProgress
	}
	epoch := op.epoch
	op.refs++
	var interrupt context.CancelFunc
	if stop {
		op.stops++
		op.epoch++
		interrupt = op.cancel
	}
	r.mu.Unlock()
	if interrupt != nil {
		interrupt()
	}

	release := func() {
		r.mu.Lock()
		op.refs--
		if stop {
			op.stops--
		}
		if op.refs == 0 {
			delete(r.lifecycle, name)
		}
		r.mu.Unlock()
	}
	select {
	case op.gate <- struct{}{}:
	case <-ctx.Done():
		release()
		return nil, nil, nil, ctx.Err()
	}
	r.mu.Lock()
	if !stop && (op.stops > 0 || epoch != op.epoch) {
		r.mu.Unlock()
		<-op.gate
		release()
		return nil, nil, nil, ErrLifecycleInProgress
	}
	opCtx, cancel := context.WithCancel(ctx)
	op.cancel = cancel
	r.mu.Unlock()
	finish := func() {
		cancel()
		r.mu.Lock()
		op.cancel = nil
		r.mu.Unlock()
		<-op.gate
		release()
	}
	return opCtx, op, finish, nil
}

func cloneAgentDef(def AgentDef) AgentDef {
	def.DisallowTools = slices.Clone(def.DisallowTools)
	def.SandboxWritableRoots = slices.Clone(def.SandboxWritableRoots)
	def.MCPServers = slices.Clone(def.MCPServers)
	for i := range def.MCPServers {
		s := &def.MCPServers[i]
		s.Args = slices.Clone(s.Args)
		s.Providers = slices.Clone(s.Providers)
		s.Env = maps.Clone(s.Env)
		s.Headers = maps.Clone(s.Headers)
	}
	return def
}

func sameLaunchDefinition(a, b AgentDef) bool {
	// These labels affect fleet policy/presentation, not the process being
	// started. Publication merges runtime fields into the newest definition.
	for _, d := range []*AgentDef{&a, &b} {
		d.AutoStart = false
		d.Parent, d.Purpose, d.Role, d.Description, d.TargetID = "", "", "", "", ""
	}
	return reflect.DeepEqual(a, b)
}

func registryConfig(def *AgentDef, requireResume bool) Config {
	return Config{
		Provider: def.Provider, WorkDir: def.WorkDir, SessionID: def.SessionID,
		RequireResume: requireResume, Model: def.Model, DisallowTools: def.DisallowTools,
		MCPServers: def.MCPServers, MCPExclusive: def.MCPExclusive,
		GrokConnect: def.GrokConnect || def.ConnectURL != "", ConnectURL: def.ConnectURL,
		ConnectPID: def.ConnectPID, SandboxMode: def.SandboxMode,
		SandboxWritableRoots: def.SandboxWritableRoots, SandboxNetworkAccess: def.SandboxNetworkAccess,
		Goal: def.Goal,
	}
}

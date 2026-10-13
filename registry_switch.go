// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SeatSwitch reports a [Registry.SwitchModel].
type SeatSwitch struct {
	// Live is true when a running seat switched in place. False means the
	// seat was not running: its definition now names the new model, and its
	// next launch resumes the same conversation on it.
	Live bool
	// Changed is false when the seat was already on that provider and model.
	Changed          bool
	From, To         Provider
	FromModel, Model string
}

// switchSettle bounds how long a forced switch waits for an interrupted turn
// to end before giving up.
const switchSettle = 30 * time.Second

// SwitchModel moves the named seat to provider and model, keeping its
// conversation (🎯T195). It is the one way a seat changes model: nothing is
// summarised, and the seat keeps its name, session and history.
//
// provider may be a plan or a sidecar id; empty keeps the seat's own. An
// empty model takes the standard session model on the destination plan.
//
// A running seat switches in place ([Agent.SwitchModel]). A turn in flight
// refuses the switch unless force is set, which interrupts the turn first. A
// seat that is not running has its definition rewritten, so its next launch
// resumes the conversation on the new model; only a sidecar seat can change
// provider that way, since a native CLI session belongs to its provider.
func (r *Registry) SwitchModel(ctx context.Context, name string, provider Provider, model string, force bool) (SeatSwitch, error) {
	def := r.Def(name)
	if def == nil {
		return SeatSwitch{}, fmt.Errorf("agent %q not registered", name)
	}
	from, fromModel := def.Provider, def.Model
	to := from
	if strings.TrimSpace(string(provider)) != "" {
		to = SubscriptionSeatProvider(provider)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		picked, err := standardSessionModel(ctx, PlanProvider(to))
		if err != nil {
			return SeatSwitch{}, fmt.Errorf("switch %s to %s: %w", name, to, err)
		}
		model = picked
	}
	out := SeatSwitch{From: from, To: to, FromModel: fromModel, Model: model}
	r.mu.Lock()
	proc := r.procs[name]
	r.mu.Unlock()
	if proc != nil && proc.Alive() {
		out.Live = true
		if PlanProvider(proc.Provider()) == PlanProvider(to) && proc.Model() == model {
			return out, nil
		}
		if proc.PromptInFlight() {
			if !force {
				return out, fmt.Errorf("switch %s: a turn is in flight; wait for it, or force to interrupt it", name)
			}
			if err := proc.Interrupt(); err != nil {
				return out, fmt.Errorf("switch %s: interrupt the turn in flight: %w", name, err)
			}
			deadline := time.Now().Add(switchSettle)
			for proc.PromptInFlight() {
				if time.Now().After(deadline) {
					return out, fmt.Errorf("switch %s: the interrupted turn did not end within %s", name, switchSettle)
				}
				select {
				case <-ctx.Done():
					return out, ctx.Err()
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
		if err := proc.SwitchModel(to, model); err != nil {
			return out, err
		}
		out.Changed = true
		return out, nil
	}
	if PlanProvider(from) == PlanProvider(to) && fromModel == model {
		return out, nil
	}
	if PlanProvider(from) != PlanProvider(to) && (!useOMP(Config{Provider: from}) || !useOMP(Config{Provider: to})) {
		return out, fmt.Errorf("switch %s: a stopped %s seat cannot move to %s; only a sidecar seat changes provider", name, from, to)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.agents[name]
	if cur == nil {
		return out, fmt.Errorf("agent %q not registered", name)
	}
	if p := r.procs[name]; p != nil && p.Alive() {
		return out, fmt.Errorf("switch %s: the seat started while switching; try again", name)
	}
	next := cloneAgentDef(*cur)
	next.Provider, next.Model = to, model
	r.agents[name] = &next
	if err := r.save(); err != nil {
		r.agents[name] = cur
		return out, fmt.Errorf("persist switched agent %q: %w", name, err)
	}
	out.Changed = true
	return out, nil
}

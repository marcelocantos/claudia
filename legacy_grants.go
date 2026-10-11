// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "slices"

// obsoleteLegacyGrants is the fixed 2026-10-10 scout inventory (T177), not
// a rule that all Claude/Grok grants are disposable. In particular, the
// non-auto-start Grok row and any newly minted legacy grant are NOT in scope.
// The broker runs this migration only once per state directory.
var obsoleteLegacyGrants = map[string]Provider{
	"jv-t254.2-merge-reconcile":       ProviderClaude,
	"jv-t832-j30-33-provider":         ProviderClaude,
	"jv-t838-j13-grok-migrate":        ProviderClaude,
	"jv-t838-merge-reconcile":         ProviderClaude,
	"jv-t840-merge-reconcile":         ProviderClaude,
	"jv-t928-probe-1":                 ProviderClaude,
	"jv-t928-probe-2":                 ProviderClaude,
	"jv-t928-probe-3":                 ProviderClaude,
	"mnemo-release-v0107":             ProviderClaude,
	"pimp-arrai-779-native":           ProviderClaude,
	"pimp-sysinfo-backlog":            ProviderClaude,
	"pimp-vellum-convert-delay-steer": ProviderClaude,
	"pimp-vellum-housekeeping-katex":  ProviderClaude,
	"pimp-xbnf-phase0":                ProviderClaude,
	"pimp-den-harness":                ProviderGrok,
	"spyder-release-0.97.0":           ProviderGrok,
}

// PruneObsoleteLegacyGrants removes only matching named, auto-start grants
// with no in-process owner. The caller must arrange one-shot execution. The owner authorized this
// exact scout inventory after verifying no external processes held the rows. A failed save restores
// the definitions in memory so the migration can be retried safely.
func (r *Registry) PruneObsoleteLegacyGrants() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := make(map[string]*AgentDef)
	for name, provider := range obsoleteLegacyGrants {
		def := r.agents[name]
		if def == nil || def.Provider != provider || !def.AutoStart || r.procs[name] != nil || r.lifecycle[name] != nil {
			continue
		}
		removed[name] = def
		delete(r.agents, name)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := r.save(); err != nil {
		for name, def := range removed {
			r.agents[name] = def
		}
		return nil, err
	}
	names := make([]string, 0, len(removed))
	for name := range removed {
		delete(r.resumeDenied, name)
		delete(r.freshSession, name)
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

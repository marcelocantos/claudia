// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestPruneObsoleteLegacyGrantsExactLiveInventory(t *testing.T) {
	if len(obsoleteLegacyGrants) != 13 {
		t.Fatalf("approved live inventory = %d, want 13", len(obsoleteLegacyGrants))
	}
	path := filepath.Join(t.TempDir(), "grants.json")
	// These names were in the older scout snapshot, but the owner reserved
	// them for personal work. They must survive even with the exact original
	// claude + auto_start fields (and without relying on the one-shot marker).
	personal := []AgentDef{
		{Name: "pimp-arrai-779-native", Provider: ProviderClaude, AutoStart: true, SessionID: "personal-arrai"},
		{Name: "pimp-vellum-housekeeping-katex", Provider: ProviderClaude, AutoStart: true, SessionID: "personal-vellum"},
		{Name: "pimp-xbnf-phase0", Provider: ProviderClaude, AutoStart: true, SessionID: "personal-xbnf"},
	}
	for _, def := range personal {
		if _, exists := obsoleteLegacyGrants[def.Name]; exists {
			t.Fatalf("personal-work seat %q must never be allowlisted", def.Name)
		}
	}
	defs := make([]AgentDef, 0, 20)
	for name, provider := range obsoleteLegacyGrants {
		defs = append(defs, AgentDef{Name: name, Provider: provider, AutoStart: true, SessionID: "legacy-" + name})
	}
	keep := []AgentDef{
		{Name: "new-claude-seat", Provider: ProviderClaude, AutoStart: true},
		{Name: "pimp-sysinfo-backlog-grok", Provider: ProviderGrok},
		{Name: "new-grok-seat", Provider: ProviderGrok, AutoStart: true},
		{Name: "jv-t928-probe-1", Provider: Provider("anthropic"), AutoStart: true, SessionID: "new-session"},
	}
	// The same name cannot coexist as two registry keys: replace the legacy
	// row in this fixture's second pass to test provider guard separately.
	defs = append(defs, keep[:3]...)
	defs = append(defs, personal...)
	body, err := json.Marshal(defs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := r.PruneObsoleteLegacyGrants()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 13 || !slices.IsSorted(removed) {
		t.Fatalf("removed = %v", removed)
	}
	for _, def := range append(keep[:3], personal...) {
		if got := r.Def(def.Name); got == nil || !reflect.DeepEqual(*got, def) {
			t.Fatalf("legitimate %q changed: %+v", def.Name, got)
		}
	}
	if len(r.List()) != 6 {
		t.Fatalf("registry still contains %d rows", len(r.List()))
	}
	reloaded, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 6 {
		t.Fatalf("persisted rows = %d", len(reloaded.List()))
	}
	if again, err := reloaded.PruneObsoleteLegacyGrants(); err != nil || len(again) != 0 {
		t.Fatalf("repeat = %v, %v", again, err)
	}
	for _, def := range []AgentDef{keep[3], {Name: "pimp-den-harness", Provider: ProviderGrok, SessionID: "new-grok"}, {Name: "spyder-release-0.97.0", Provider: ProviderGrok, SessionID: "stopped-grok", AutoStart: false}} {
		if err := reloaded.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := reloaded.PruneObsoleteLegacyGrants(); err != nil || len(again) != 0 {
		t.Fatalf("guard = %v, %v", again, err)
	}
}

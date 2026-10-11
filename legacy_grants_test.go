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

func TestPruneObsoleteLegacyGrantsExactScoutInventory(t *testing.T) {
	if len(obsoleteLegacyGrants) != 16 {
		t.Fatalf("scout inventory = %d, want 16", len(obsoleteLegacyGrants))
	}
	path := filepath.Join(t.TempDir(), "grants.json")
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
	if len(removed) != 16 || !slices.IsSorted(removed) {
		t.Fatalf("removed = %v", removed)
	}
	for _, def := range keep[:3] {
		if got := r.Def(def.Name); got == nil || !reflect.DeepEqual(*got, def) {
			t.Fatalf("legitimate %q changed: %+v", def.Name, got)
		}
	}
	if len(r.List()) != 3 {
		t.Fatalf("registry still contains %d rows", len(r.List()))
	}
	reloaded, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 3 {
		t.Fatalf("persisted rows = %d", len(reloaded.List()))
	}
	if again, err := reloaded.PruneObsoleteLegacyGrants(); err != nil || len(again) != 0 {
		t.Fatalf("repeat = %v, %v", again, err)
	}
	for _, def := range []AgentDef{keep[3], {Name: "pimp-den-harness", Provider: ProviderGrok, SessionID: "new-grok"}} {
		if err := reloaded.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := reloaded.PruneObsoleteLegacyGrants(); err != nil || len(again) != 0 {
		t.Fatalf("guard = %v, %v", again, err)
	}
}

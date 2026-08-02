// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewRegistryEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	r, err := NewRegistry(path)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List on empty registry: got %d entries, want 0", len(got))
	}
}

func TestNewRegistryLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	// Pre-populate the file with a known definition.
	defs := []AgentDef{{
		Name:      "alice",
		WorkDir:   "/tmp/alice",
		SessionID: "session-alice",
		Model:     "opus",
		AutoStart: true,
	}}
	data, _ := json.MarshalIndent(defs, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	r, err := NewRegistry(path)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	def := r.Def("alice")
	if def == nil {
		t.Fatal("Def(alice) = nil, want populated")
	}
	if def.SessionID != "session-alice" {
		t.Errorf("SessionID = %q, want session-alice", def.SessionID)
	}
	if def.Model != "opus" {
		t.Errorf("Model = %q, want opus", def.Model)
	}
	if !def.AutoStart {
		t.Error("AutoStart = false, want true")
	}
}

func TestNewRegistryMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := NewRegistry(path); err == nil {
		t.Error("NewRegistry on malformed file returned nil, want error")
	}
}

func TestRegistryRegisterRequiresSessionID(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	err = r.Register(AgentDef{Name: "bob", WorkDir: "/tmp/bob"})
	if err == nil {
		t.Error("Register without SessionID returned nil, want error")
	}
}

func TestRegistryRegisterPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r, err := NewRegistry(path)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	def := AgentDef{
		Name:      "carol",
		WorkDir:   "/tmp/carol",
		SessionID: "session-carol",
		Parent:    "overseer",
		Purpose:   PurposeAside,
	}
	if err := r.Register(def); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Reload from disk and verify.
	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatalf("NewRegistry reload: %v", err)
	}
	got := r2.Def("carol")
	if got == nil {
		t.Fatal("Def(carol) on reload = nil")
	}
	if got.SessionID != "session-carol" {
		t.Errorf("SessionID after reload = %q, want session-carol", got.SessionID)
	}
	if got.Parent != "overseer" {
		t.Errorf("Parent after reload = %q, want overseer", got.Parent)
	}
	if got.Purpose != PurposeAside {
		t.Errorf("Purpose after reload = %q, want %q", got.Purpose, PurposeAside)
	}
}

func TestRegistryList(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(filepath.Join(dir, "registry.json"))

	_ = r.Register(AgentDef{Name: "a", WorkDir: "/a", SessionID: "sid-a"})
	_ = r.Register(AgentDef{Name: "b", WorkDir: "/b", SessionID: "sid-b"})

	list := r.List()
	if len(list) != 2 {
		t.Errorf("List = %d entries, want 2", len(list))
	}
}

func TestRegistryRemove(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(filepath.Join(dir, "registry.json"))
	_ = r.Register(AgentDef{Name: "doomed", WorkDir: "/x", SessionID: "sid-x"})

	if err := r.Remove("doomed"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if r.Def("doomed") != nil {
		t.Error("Def after Remove = populated, want nil")
	}
}

func TestEnsureAgentCreatesNew(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(filepath.Join(dir, "registry.json"))

	def, err := r.EnsureAgent("fresh", "/tmp/fresh", "sonnet", true)
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if def.Name != "fresh" {
		t.Errorf("Name = %q, want fresh", def.Name)
	}
	if def.SessionID == "" {
		t.Error("SessionID empty after EnsureAgent")
	}
	if !def.AutoStart {
		t.Error("AutoStart = false, want true")
	}
}

// Launch must honour AgentDef.Provider — empty still means Claude, but a
// non-Claude provider must not be silently dropped (that left Grok Session
// unreachable via the registry after v0.16 shipped ACP Start).
func TestLaunchPassesProvider(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name:      "codex-sess",
		WorkDir:   t.TempDir(),
		SessionID: "sid-x",
		Provider:  ProviderCodex,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Launch("codex-sess")
	if err == nil {
		t.Fatal("Launch with ProviderCodex returned nil error; want experimental CapabilityError")
	}
	capErr, ok := err.(*CapabilityError)
	if !ok {
		t.Fatalf("err type %T, want *CapabilityError: %v", err, err)
	}
	if capErr.Provider != ProviderCodex || capErr.Capability != "session" {
		t.Fatalf("CapabilityError = %+v, want ProviderCodex session", capErr)
	}
}

func TestEnsureAgentReturnsExisting(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(filepath.Join(dir, "registry.json"))

	first, _ := r.EnsureAgent("pinned", "/tmp/pinned", "opus", false)
	second, err := r.EnsureAgent("pinned", "/tmp/pinned", "opus", false)
	if err != nil {
		t.Fatalf("EnsureAgent second call: %v", err)
	}
	if first.SessionID != second.SessionID {
		t.Errorf("SessionID changed between calls: %q then %q",
			first.SessionID, second.SessionID)
	}
}

func TestEnsureAgentSameWorkdirDifferentNamesAreIndependent(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(filepath.Join(dir, "registry.json"))

	// Two fleet workers often share a repo workdir under different names.
	// EnsureAgent must mint two defs/sessions and leave the original name
	// alone (no workdir-based session steal / silent rename).
	const workDir = "/tmp/proj"
	first, err := r.EnsureAgent("old-name", workDir, "sonnet", false)
	if err != nil {
		t.Fatalf("EnsureAgent first: %v", err)
	}
	second, err := r.EnsureAgent("new-name", workDir, "sonnet", false)
	if err != nil {
		t.Fatalf("EnsureAgent second: %v", err)
	}
	if second.Name != "new-name" {
		t.Errorf("second.Name = %q, want new-name", second.Name)
	}
	if second.SessionID == first.SessionID {
		t.Errorf("SessionIDs collided: both %q", first.SessionID)
	}
	if second.SessionID == "" {
		t.Error("second SessionID empty")
	}
	still := r.Def("old-name")
	if still == nil {
		t.Fatal("old-name missing from registry after second EnsureAgent")
	}
	if still.Name != "old-name" {
		t.Errorf("old-name silently renamed to %q", still.Name)
	}
	if still.SessionID != first.SessionID {
		t.Errorf("old-name SessionID changed: %q → %q", first.SessionID, still.SessionID)
	}
	if r.Def("new-name") == nil {
		t.Fatal("new-name not registered")
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
}

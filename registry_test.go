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

func TestEnsureAgentRenamesOnWorkdirCollision(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(filepath.Join(dir, "registry.json"))

	// Register under one name, then EnsureAgent with a different name
	// but the same workdir — the existing definition should be renamed
	// and its SessionID preserved.
	original, _ := r.EnsureAgent("old-name", "/tmp/proj", "sonnet", false)
	renamed, err := r.EnsureAgent("new-name", "/tmp/proj", "sonnet", false)
	if err != nil {
		t.Fatalf("EnsureAgent rename: %v", err)
	}
	if renamed.Name != "new-name" {
		t.Errorf("Name = %q, want new-name", renamed.Name)
	}
	if renamed.SessionID != original.SessionID {
		t.Errorf("SessionID changed on rename: %q → %q",
			original.SessionID, renamed.SessionID)
	}
	if r.Def("old-name") != nil {
		t.Error("old name still in registry after rename")
	}
}

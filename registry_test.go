// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		TargetID:  "T10.2",
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
	if got.TargetID != "T10.2" {
		t.Errorf("TargetID after reload = %q, want T10.2", got.TargetID)
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
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("CODEX_BIN", writeFakeCodexAppServer(t))
	if err := r.Register(AgentDef{
		Name:      "codex-sess",
		WorkDir:   t.TempDir(),
		SessionID: "sid-x",
		Provider:  ProviderCodex,
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := r.Launch("codex-sess")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer agent.Stop()
	if agent == nil {
		t.Fatal("Launch returned nil agent")
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

// installFakeRegistryStart swaps registryStart for a backend that succeeds
// without spawning Claude, and restores the original on cleanup. Returns a
// pointer that receives each Config passed to Launch.
func installFakeRegistryStart(t *testing.T) *[]Config {
	t.Helper()
	var cfgs []Config
	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		cfgs = append(cfgs, cfg)
		backend := &fakeAgentBackend{name: "fake-claude"}
		return startWithBackend(cfg, backend)
	}
	return &cfgs
}

func writeClaudeSessionJSONL(t *testing.T, sessionID, workDir string) {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	path := SessionJSONLPath(sessionID, workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// 🎯T15 (a): Start success without JSONL must leave Materialized false so a
// never-materialized seat can re-launch without RequireResume hard-fail.
func TestHermeticLaunchDoesNotMaterializeWithoutJSONL(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	cfgs := installFakeRegistryStart(t)
	workDir := t.TempDir()
	regPath := filepath.Join(t.TempDir(), "registry.json")
	r, err := NewRegistry(regPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	const name = "seat-a"
	const sid = "never-wrote-jsonl"
	if err := r.Register(AgentDef{Name: name, WorkDir: workDir, SessionID: sid}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	agent, err := r.Launch(name)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer agent.Stop()
	r.Stop(name)

	def := r.Def(name)
	if def == nil {
		t.Fatal("Def nil after Launch")
	}
	if def.Materialized {
		t.Error("Materialized = true after Start without JSONL; want false")
	}
	if len(*cfgs) != 1 {
		t.Fatalf("Start calls = %d, want 1", len(*cfgs))
	}
	if (*cfgs)[0].RequireResume {
		t.Error("first Launch set RequireResume=true; want false for never-materialized")
	}

	// Re-launch still allowed without JSONL (no fail-closed).
	agent2, err := r.Launch(name)
	if err != nil {
		t.Fatalf("second Launch (never materialized): %v", err)
	}
	agent2.Stop()
	r.Stop(name)
	if r.Def(name).Materialized {
		t.Error("Materialized flipped true on second bare Launch")
	}
	if len(*cfgs) != 2 {
		t.Fatalf("Start calls after re-launch = %d, want 2", len(*cfgs))
	}
	if (*cfgs)[1].RequireResume {
		t.Error("second Launch set RequireResume=true; want false")
	}
}

// 🎯T15 (b): after JSONL / MarkMaterialized evidence, Materialized is true and
// subsequent Launch sets RequireResume.
func TestHermeticMaterializeFromJSONLAndRequireResume(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	cfgs := installFakeRegistryStart(t)
	workDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	const name = "seat-b"
	const sid = "jsonl-evidence-session"
	if err := r.Register(AgentDef{Name: name, WorkDir: workDir, SessionID: sid}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Bare Start: not materialized.
	agent, err := r.Launch(name)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	agent.Stop()
	r.Stop(name)
	if r.Def(name).Materialized {
		t.Fatal("Materialized true before JSONL evidence")
	}

	// Mark without JSONL must refuse (Claude).
	if err := r.MarkMaterialized(name); err == nil {
		t.Fatal("MarkMaterialized without JSONL returned nil, want error")
	}

	// Durable transcript appears (first completed turn).
	writeClaudeSessionJSONL(t, sid, workDir)

	if err := r.MarkMaterialized(name); err != nil {
		t.Fatalf("MarkMaterialized after JSONL: %v", err)
	}
	if !r.Def(name).Materialized {
		t.Fatal("Materialized false after MarkMaterialized with JSONL")
	}

	// Next Launch must RequireResume.
	agent2, err := r.Launch(name)
	if err != nil {
		t.Fatalf("Launch after materialize: %v", err)
	}
	agent2.Stop()
	r.Stop(name)
	if len(*cfgs) < 2 {
		t.Fatalf("Start calls = %d, want ≥2", len(*cfgs))
	}
	last := (*cfgs)[len(*cfgs)-1]
	if !last.RequireResume {
		t.Error("Launch after materialize: RequireResume=false, want true")
	}

	// Launch with JSONL already present also promotes Materialized without
	// an explicit MarkMaterialized (rehydrate path).
	r2, err := NewRegistry(filepath.Join(t.TempDir(), "registry2.json"))
	if err != nil {
		t.Fatalf("NewRegistry r2: %v", err)
	}
	const name2 = "seat-b-promote"
	const sid2 = "already-has-jsonl"
	writeClaudeSessionJSONL(t, sid2, workDir)
	if err := r2.Register(AgentDef{Name: name2, WorkDir: workDir, SessionID: sid2}); err != nil {
		t.Fatalf("Register r2: %v", err)
	}
	agent3, err := r2.Launch(name2)
	if err != nil {
		t.Fatalf("Launch promote: %v", err)
	}
	agent3.Stop()
	r2.Stop(name2)
	if !r2.Def(name2).Materialized {
		t.Error("Launch with existing JSONL left Materialized false; want promote")
	}
}

// 🎯T15 (c): RequireResume still fails closed when Materialized && JSONL missing.
func TestHermeticMaterializedRequireResumeFailsClosed(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	// Use real Start path (no fake) so RequireResume fail-closed in
	// startWithBackend runs before any backend spawn — same as
	// TestHermeticClaudeRequireResumeFailsClosed, wired through Registry.
	workDir := t.TempDir()
	r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	const name = "seat-c"
	if err := r.Register(AgentDef{
		Name:         name,
		WorkDir:      workDir,
		SessionID:    "materialized-but-jsonl-gone",
		Materialized: true,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err = r.Launch(name)
	if err == nil {
		t.Fatal("Launch with Materialized and missing JSONL returned nil")
	}
	if !strings.Contains(err.Error(), "refusing to mint") {
		t.Errorf("error = %v, want refuse-to-mint wording", err)
	}
}

func TestMarkMaterializedUnknownAgent(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.MarkMaterialized("ghost"); err == nil {
		t.Error("MarkMaterialized unknown returned nil, want error")
	}
}

func TestLaunchLatchesCursorResumeDenied(t *testing.T) {
	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	var starts int
	registryStart = func(context.Context, Config) (*Agent, error) {
		starts++
		return nil, fmt.Errorf("acp session/load sid: connection closed (%w)", ErrCursorResumeDenied)
	}

	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name:      "jevons",
		WorkDir:   t.TempDir(),
		SessionID: "sid-cursor",
		Provider:  ProviderCursor,
		AutoStart: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Launch("jevons"); !IsCursorResumeDenied(err) {
		t.Fatalf("first Launch: %v", err)
	}
	if _, err := r.Launch("jevons"); !IsCursorResumeDenied(err) {
		t.Fatalf("second Launch: %v", err)
	}
	if starts != 1 {
		t.Fatalf("starts=%d want 1 (latched fail-closed must not spawn again)", starts)
	}
	if r.ResumeDenied("jevons") == nil {
		t.Fatal("expected ResumeDenied latch")
	}
}

// 🎯T545.1 / T57: a Grok/Codex/Cursor row reloaded from disk is a bounce resume, not a
// never-materialized mint. Launch must RequireResume and must not persist
// a replacement session_id.
func TestHermeticCodexCursorBounceLaunchRequiresResume(t *testing.T) {
	cases := []struct {
		name     string
		agent    string
		sid      string
		provider Provider
	}{
		{"codex", "jv-t543-compact-once", "01a030e9-054a-45c0-8c2f-afc545afa986", ProviderCodex},
		{"cursor", "jevons-po", "531d90af-9299-4607-ab3d-7dcc14fa7a83", ProviderCursor},
		{"grok", "jevons", "01a030e9-existing-grok", ProviderGrok},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.json")
			workDir := t.TempDir()
			r1, err := NewRegistry(path)
			if err != nil {
				t.Fatalf("NewRegistry seed: %v", err)
			}
			if err := r1.Register(AgentDef{
				Name: tc.agent, WorkDir: workDir, SessionID: tc.sid,
				Provider: tc.provider, AutoStart: true,
			}); err != nil {
				t.Fatalf("Register: %v", err)
			}

			r2, err := NewRegistry(path)
			if err != nil {
				t.Fatalf("NewRegistry bounce: %v", err)
			}
			prev := registryStart
			t.Cleanup(func() { registryStart = prev })
			var cfgs []Config
			registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
				cfgs = append(cfgs, cfg)
				if cfg.RequireResume {
					return nil, fmt.Errorf("session %s: existing conversation required — refusing to mint a replacement session", cfg.SessionID)
				}
				reminted := cfg
				reminted.SessionID = tc.sid + "-reminted"
				return startWithBackend(reminted, &fakeAgentBackend{name: "fake-" + tc.name})
			}

			_, err = r2.Launch(tc.agent)
			if err == nil {
				t.Fatal("bounce Launch reminted; want RequireResume fail-closed")
			}
			if !strings.Contains(err.Error(), "refusing to mint") {
				t.Errorf("error = %v, want refuse-to-mint wording", err)
			}
			if len(cfgs) != 1 || !cfgs[0].RequireResume {
				t.Fatalf("Start RequireResume = %+v, want true", cfgs)
			}
			if got := r2.Def(tc.agent).SessionID; got != tc.sid {
				t.Fatalf("persisted session_id = %q, want %q", got, tc.sid)
			}
		})
	}
}

func TestHermeticCodexLaunchRefusesRemintPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	workDir := t.TempDir()
	const name = "codex-worker"
	const sid = "01a030e9-keep-me"

	r1, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Register(AgentDef{
		Name: name, WorkDir: workDir, SessionID: sid,
		Provider: ProviderCodex,
	}); err != nil {
		t.Fatal(err)
	}

	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		reminted := cfg
		reminted.SessionID = "01a030f5-should-not-persist"
		return startWithBackend(reminted, &fakeAgentBackend{name: "fake-codex"})
	}

	_, err = r2.Launch(name)
	if err == nil {
		t.Fatal("Launch persisted a remint; want refuse")
	}
	if !strings.Contains(err.Error(), "refusing remint") {
		t.Errorf("error = %v, want refusing remint", err)
	}
	if got := r2.Def(name).SessionID; got != sid {
		t.Fatalf("session_id drifted to %q", got)
	}
	r3, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := r3.Def(name).SessionID; got != sid {
		t.Fatalf("disk session_id = %q after remint refuse, want %q", got, sid)
	}
}

func TestHermeticCodexEnsureAgentFirstLaunchMayMint(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	def, err := r.EnsureAgent("fresh-codex", t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name: def.Name, WorkDir: def.WorkDir, SessionID: def.SessionID,
		Provider: ProviderCodex,
	}); err != nil {
		t.Fatal(err)
	}

	prev := registryStart
	t.Cleanup(func() { registryStart = prev })
	registryStart = func(_ context.Context, cfg Config) (*Agent, error) {
		if cfg.RequireResume {
			return nil, fmt.Errorf("first mint set RequireResume")
		}
		reminted := cfg
		reminted.SessionID = "01a030e9-first-thread"
		return startWithBackend(reminted, &fakeAgentBackend{name: "fake-codex"})
	}

	agent, err := r.Launch("fresh-codex")
	if err != nil {
		t.Fatalf("first Launch: %v", err)
	}
	agent.Stop()
	r.Stop("fresh-codex")
	if got := r.Def("fresh-codex").SessionID; got != "01a030e9-first-thread" {
		t.Fatalf("first mint session_id = %q, want Codex thread", got)
	}
}

func TestMarkMaterializedGrokHostAttestation(t *testing.T) {
	// Non-Claude providers have no Claude JSONL; host may attest after a turn.
	r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Register(AgentDef{
		Name:      "grok-seat",
		WorkDir:   t.TempDir(),
		SessionID: "grok-sid",
		Provider:  ProviderGrok,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.MarkMaterialized("grok-seat"); err != nil {
		t.Fatalf("MarkMaterialized Grok: %v", err)
	}
	if !r.Def("grok-seat").Materialized {
		t.Error("Grok MarkMaterialized left Materialized false")
	}
}

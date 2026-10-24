// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestValidateChildProcessEnvRejectsMalformed(t *testing.T) {
	cases := []ChildProcessEnv{
		{"": "x"},
		{"BAD=KEY": "x"},
		{"GOOD": "\x00"},
		{"1START": "x"},
	}
	for _, env := range cases {
		if err := ValidateChildProcessEnv(env); err == nil {
			t.Fatalf("expected error for %#v", env)
		}
	}
	if err := ValidateChildProcessEnv(ChildProcessEnv{"CLAUDIA_T218": "ok", "PATH": "/custom/bin"}); err != nil {
		t.Fatal(err)
	}
}

func TestCloneChildProcessEnvIsolatesMaps(t *testing.T) {
	a := CloneChildProcessEnv(ChildProcessEnv{"A": "1"})
	b := CloneChildProcessEnv(ChildProcessEnv{"B": "2"})
	a["A"] = "mutated"
	if b["B"] != "2" {
		t.Fatalf("b leaked: %#v", b)
	}
}

func TestRegistryLoadRestoreRetainsChildProcessEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	env := ChildProcessEnv{"CLAUDIA_T218": "seat-a", "PATH": "/tmp/t218-a/bin"}
	if err := reg.Register(AgentDef{Name: "seat", SessionID: "sid", ChildProcessEnv: env}); err != nil {
		t.Fatal(err)
	}
	restored, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	def := restored.Def("seat")
	if def == nil {
		t.Fatal("missing def")
	}
	if def.ChildProcessEnv["CLAUDIA_T218"] != "seat-a" || def.ChildProcessEnv["PATH"] != "/tmp/t218-a/bin" {
		t.Fatalf("restore: %#v", def.ChildProcessEnv)
	}
	env["CLAUDIA_T218"] = "mutated"
	if restored.Def("seat").ChildProcessEnv["CLAUDIA_T218"] != "seat-a" {
		t.Fatal("registry def must not alias caller map")
	}
}

func TestGrantWireRoundTripChildProcessEnv(t *testing.T) {
	cfg := Config{
		Name: "w", WorkDir: t.TempDir(), SessionID: "sid",
		ChildProcessEnv: ChildProcessEnv{"MARKER": "grant"},
	}
	raw, err := EncodeGrantDefinition(configToGrantDef("w", cfg, nil))
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeGrantDefinition(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.ChildProcessEnv["MARKER"] != "grant" {
		t.Fatalf("grant wire: %#v", back.ChildProcessEnv)
	}
	got := registryConfig(&back.AgentDef, false)
	if got.ChildProcessEnv["MARKER"] != "grant" {
		t.Fatalf("registryConfig: %#v", got.ChildProcessEnv)
	}
}

func TestNormalizeSeatConfigRejectsBadChildProcessEnv(t *testing.T) {
	_, err := normalizeSeatConfig(Config{Provider: ProviderCursor, ChildProcessEnv: ChildProcessEnv{"": "x"}})
	if err == nil {
		t.Fatal("expected refusal")
	}
}

func TestRegisterRejectsMalformedChildProcessEnv(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = reg.Register(AgentDef{Name: "x", SessionID: "s", ChildProcessEnv: ChildProcessEnv{"BAD=": "v"}})
	if err == nil {
		t.Fatal("expected register refusal")
	}
}

func TestRegistryJSONRoundTripChildProcessEnv(t *testing.T) {
	def := AgentDef{Name: "n", SessionID: "s", ChildProcessEnv: ChildProcessEnv{"K": "v"}}
	data, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var back AgentDef
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.ChildProcessEnv["K"] != "v" {
		t.Fatalf("%#v", back.ChildProcessEnv)
	}
}

func TestOmpChildProcessEnvNilWhenEmpty(t *testing.T) {
	if ompChildProcessEnv(Config{}) != nil {
		t.Fatal("expected nil")
	}
}

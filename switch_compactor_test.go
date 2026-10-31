// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

func TestSwitchModelPassesCompactOnWire(t *testing.T) {
	f := startFakeSidecar(t, []string{`{"type":"text","text":"ok"}`})
	dir := t.TempDir()
	agent, err := StartDirect(Config{
		Name: "sw-compact", WorkDir: dir, Provider: Provider(omp.Anthropic), Model: "claude-opus", TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.SwitchModel(ProviderCodex, "gpt-5.5", SwitchOptions{CompactOn: CompactOnDestination}); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	loads := f.loaded()
	if len(loads) < 2 {
		t.Fatalf("loads=%d, want switch load", len(loads))
	}
	last := loads[len(loads)-1]
	if last.CompactOn != "destination" {
		t.Fatalf("CompactOn=%q, want destination", last.CompactOn)
	}
	if last.RequestID == "" {
		t.Fatal("switch load missing request_id")
	}
}

func TestSwitchOptionsValidateExplicit(t *testing.T) {
	if err := (SwitchOptions{CompactOn: CompactOnExplicit}).Validate(); err == nil {
		t.Fatal("expected error for missing explicit model")
	}
}

func TestSwitchOptionsFromWire(t *testing.T) {
	o, err := SwitchOptionsFromWire("destination", "", "")
	if err != nil || o.CompactOn != CompactOnDestination {
		t.Fatalf("destination: o=%+v err=%v", o, err)
	}
	_, err = SwitchOptionsFromWire("bogus", "", "")
	if err == nil {
		t.Fatal("expected error for unknown compact_on")
	}
}

func TestSwitchModelRejectsMalformedExplicit(t *testing.T) {
	a := &Agent{
		ops: agentOps{
			switchModel: func(*Agent, Provider, string, SwitchOptions) error {
				t.Fatal("switchModel must not run when options are invalid")
				return nil
			},
		},
	}
	err := a.SwitchModel(ProviderCodex, "gpt-5.5", SwitchOptions{CompactOn: CompactOnExplicit})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestSwitchExplicitUsesOpTokenNotLoadForCompactorAuth(t *testing.T) {
	f := startFakeSidecar(t, nil)
	dir := t.TempDir()
	agent, err := StartDirect(Config{
		Name: "sw-tok", WorkDir: dir, Provider: Provider(omp.Anthropic), Model: "claude-opus", TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.SwitchModel(ProviderCodex, "gpt-5.5", SwitchOptions{
		CompactOn: CompactOnExplicit, CompactProvider: Provider(omp.Anthropic), CompactModel: "claude-opus",
	}); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	var tokenOps, switchLoads int
	var switchLoad omp.Message
	for _, m := range f.incoming() {
		if m.Op == omp.OpToken && m.Provider == omp.Anthropic && m.Token == "plan-token" {
			tokenOps++
		}
		if m.Op == omp.OpLoad && m.RequestID != "" {
			switchLoads++
			switchLoad = m
		}
	}
	if tokenOps != 1 {
		t.Fatalf("OpToken for compactor plan: got %d", tokenOps)
	}
	if switchLoads != 1 {
		t.Fatalf("switch loads: got %d", switchLoads)
	}
	if switchLoad.Token != "codex-token" {
		t.Fatalf("switch load token=%q, want destination plan token only", switchLoad.Token)
	}
	if switchLoad.CompactOn != "explicit" || switchLoad.CompactProvider != omp.Anthropic {
		t.Fatalf("switch load compact fields: %+v", switchLoad)
	}
	raw, _ := json.Marshal(switchLoad)
	if strings.Contains(string(raw), "plan-token") {
		t.Fatalf("compactor bearer token must not ride on switch load: %s", raw)
	}
}

func TestSwitchModelExplicitOnWire(t *testing.T) {
	f := startFakeSidecar(t, nil)
	dir := t.TempDir()
	agent, err := StartDirect(Config{
		Name: "sw-explicit", WorkDir: dir, Provider: Provider(omp.Anthropic), Model: "claude-opus", TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.SwitchModel(ProviderCodex, "gpt-5.5", SwitchOptions{
		CompactOn: CompactOnExplicit, CompactProvider: Provider(omp.Anthropic), CompactModel: "claude-opus",
	}); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	last := f.loaded()[len(f.loaded())-1]
	if last.CompactOn != "explicit" || last.CompactProvider != omp.Anthropic || last.CompactModel != "claude-opus" {
		t.Fatalf("load compact fields: %+v", last)
	}
}

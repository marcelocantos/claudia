// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
package claudia

import (
	"testing"
	"time"
)

func TestInstallBrokerUsageOverrideRejectsUnknownAndEmpty(t *testing.T) {
	t.Cleanup(ClearBrokerUsageOverride)
	if err := InstallBrokerUsageOverride(nil); err == nil {
		t.Fatal("empty allowlist")
	}
	if err := InstallBrokerUsageOverride([]Provider{Provider("nope")}); err == nil {
		t.Fatal("unknown provider")
	}
	if err := InstallBrokerUsageOverride([]Provider{ProviderGrok, ProviderGrok}); err == nil {
		t.Fatal("duplicate")
	}
}

func TestStoreBrokerUsageOverrideRefusesUnenrolledProvider(t *testing.T) {
	t.Cleanup(ClearBrokerUsageOverride)
	if err := InstallBrokerUsageOverride([]Provider{ProviderGrok}); err != nil {
		t.Fatal(err)
	}
	claude := PlanUsage{Provider: ProviderClaude, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: floatPtr(90)}}}
	if err := StoreBrokerUsageOverride([]PlanUsage{claude}); err == nil {
		t.Fatal("unenrolled provider stored")
	}
	if _, ok := lookupBrokerUsageOverride(ProviderGrok); ok {
		t.Fatal("failed store mutated readings")
	}
}

func TestLookupBrokerUsageOverride(t *testing.T) {
	t.Cleanup(ClearBrokerUsageOverride)
	if err := InstallBrokerUsageOverride([]Provider{ProviderGrok}); err != nil {
		t.Fatal(err)
	}
	remaining, used := 30.0, 70.0
	reset := time.Now().UTC().Add(24 * time.Hour)
	grok := PlanUsage{Provider: ProviderGrok, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &remaining, UsedPercent: &used, ResetsAt: &reset}}}
	if err := StoreBrokerUsageOverride([]PlanUsage{grok}); err != nil {
		t.Fatal(err)
	}
	got, ok := lookupBrokerUsageOverride(ProviderGrok)
	if !ok || got.Provider != ProviderGrok || got.Windows[0].RemainingPercent == nil || *got.Windows[0].RemainingPercent != 30 {
		t.Fatalf("lookup grok = %+v ok=%v", got, ok)
	}
	if _, ok := lookupBrokerUsageOverride(ProviderClaude); ok {
		t.Fatal("unenrolled lookup")
	}
}

func TestBrokerUsageOverrideAllows(t *testing.T) {
	t.Cleanup(ClearBrokerUsageOverride)
	if brokerUsageOverrideAllows(ProviderGrok) {
		t.Fatal("empty allowlist")
	}
	if err := InstallBrokerUsageOverride([]Provider{ProviderGrok}); err != nil {
		t.Fatal(err)
	}
	if !brokerUsageOverrideAllows(ProviderGrok) || brokerUsageOverrideAllows(ProviderClaude) {
		t.Fatal("allowlist mismatch")
	}
}

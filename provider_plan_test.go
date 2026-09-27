// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "testing"

func TestPlanProviderMapsSidecarSeatsToTheirSubscriptions(t *testing.T) {
	for _, tc := range []struct {
		seat Provider
		plan Provider
	}{
		{"xai-oauth", ProviderGrok},
		{"anthropic", ProviderClaude},
		{"openai-codex", ProviderCodex},
		{ProviderCursor, ProviderCursor},
		{ProviderGrok, ProviderGrok},
	} {
		if got := PlanProvider(tc.seat); got != tc.plan {
			t.Errorf("PlanProvider(%q) = %q, want %q", tc.seat, got, tc.plan)
		}
		if got := PlanProvider(SubscriptionSeatProvider(tc.plan)); got != tc.plan {
			t.Errorf("subscription round trip for %q = %q", tc.plan, got)
		}
	}
}

func TestSidecarProviderCanMigrateWithoutInheritingCLIClaims(t *testing.T) {
	for _, provider := range []Provider{"xai-oauth", "anthropic", "openai-codex"} {
		if err := CheckCapability(provider, CapabilityMigrate); err != nil {
			t.Errorf("%s migrate: %v", provider, err)
		}
		if got := ProviderCapabilityStatus(provider, CapabilityExtraArgs); got != CapabilityUnsupported {
			t.Errorf("%s extra args = %s, want unsupported", provider, got)
		}
	}
}

func TestSidecarIdentityMigratesThroughClaudiaAgent(t *testing.T) {
	src, _ := startMigrateFixture(t, "xai-oauth", "fake-sidecar-grok")
	src.PublishEvent(Event{Type: "user", Text: "continue this work"})
	dest := &fakeAgentBackend{name: "fake-sidecar-claude", assignedSession: "claude-dest"}
	if err := src.migrateWithBackend(&MigrateArgs{Provider: "anthropic"}, dest); err != nil {
		t.Fatal(err)
	}
	if src.Provider() != "anthropic" || src.SessionID() != "claude-dest" {
		t.Fatalf("migrated seat = %q / %q", src.Provider(), src.SessionID())
	}
}

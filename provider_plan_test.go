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

func TestSidecarProviderDoesNotInheritCLIClaims(t *testing.T) {
	for _, provider := range []Provider{"xai-oauth", "anthropic", "openai-codex"} {
		if got := ProviderCapabilityStatus(provider, CapabilityExtraArgs); got != CapabilityUnsupported {
			t.Errorf("%s extra args = %s, want unsupported", provider, got)
		}
	}
}

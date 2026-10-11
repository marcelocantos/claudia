// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"strings"
	"testing"
)

// TestGrokCapabilityMatrixIsExplicit pins the published status of every
// capability claudia reports on for Grok. As with Codex, the point is not
// that the values are these values; it is that each one is *stated*, so a
// Grok gap is read off the matrix instead of discovered at runtime by the
// caller it bites.
func TestGrokToolRestrictionsReasonNamesTheRealMechanism(t *testing.T) {
	err := CheckCapability(ProviderGrok, CapabilityToolRestrictions)
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("CheckCapability(grok, tool_restrictions) = %T %v, want *CapabilityError", err, err)
	}
	for _, want := range []string{"--deny", "--disallowed-tools", "bypassPermissions"} {
		if !strings.Contains(capErr.Reason, want) {
			t.Errorf("reason does not mention %q, so it reads as a gap in grok "+
				"rather than in claudia: %q", want, capErr.Reason)
		}
	}
	if strings.Contains(capErr.Reason, "no per-tool disallow flag") {
		t.Errorf("reason copies the Codex rationale, which is false of grok: %q", capErr.Reason)
	}
}

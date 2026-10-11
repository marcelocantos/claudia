// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestTaskToolPolicyFailsClosed(t *testing.T) {
	policy := &TaskToolPolicy{Builtins: []string{"Read"}, MaxTurns: 1}
	for _, provider := range []Provider{ProviderCodex, ProviderCursor, ProviderBedrock, ProviderOllama} {
		var capErr *CapabilityError
		if err := validateTaskToolPolicy(provider, policy); !errors.As(err, &capErr) || capErr.Capability != CapabilityToolRestrictions {
			t.Errorf("%s accepted restricted task: %v", provider, err)
		}
	}
	if err := validateTaskToolPolicy(ProviderGrok, policy); err == nil || !strings.Contains(err.Error(), "isolated home") {
		t.Fatalf("Grok without isolated home: %v", err)
	}
	publicHome := t.TempDir()
	if err := os.Chmod(publicHome, 0o755); err != nil {
		t.Fatal(err)
	}
	policy.HomeDir = publicHome
	if err := validateTaskToolPolicy(ProviderGrok, policy); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("Grok with public home: %v", err)
	}
}

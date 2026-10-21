// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
)

func TestCursorCapabilityMatrixIsExplicit(t *testing.T) {
	want := map[Capability]CapabilityStatus{
		CapabilityTask:             CapabilityUnsupported,
		CapabilityResume:           CapabilitySupported,
		CapabilitySession:          CapabilitySupported,
		CapabilityRewind:           CapabilityUnsupported,
		CapabilityCost:             CapabilityUnsupported,
		CapabilityTmuxAttach:       CapabilityUnsupported,
		CapabilityTerminalLog:      CapabilityUnsupported,
		CapabilityPermissionMode:   CapabilityUnsupported,
		CapabilityToolRestrictions: CapabilityUnsupported,
		CapabilityImageInput:       CapabilityUnsupported,
		CapabilityWebSearch:        CapabilityUnsupported,
		CapabilitySandboxPolicy:    CapabilityUnsupported,
		CapabilityExtraArgs:        CapabilityUnsupported,
		CapabilityModelSwitch:      CapabilitySupported,
		CapabilityMCPTools:         CapabilitySupported, // sidecar binds tools itself; T118 was the ACP transport
	}
	got := ProviderCapabilityMatrix(ProviderCursor)
	if len(got) != len(want) {
		t.Errorf("matrix has %d entries, want %d: %+v", len(got), len(want), got)
	}
	for capability, wantStatus := range want {
		if got[capability] != wantStatus {
			t.Errorf("cursor %s = %q, want %q", capability, got[capability], wantStatus)
		}
		if wantStatus == CapabilitySupported {
			continue
		}
		if reason := ProviderCapabilityReason(ProviderCursor, capability); reason == "" {
			t.Errorf("cursor %s is %q with no documented reason", capability, wantStatus)
		}
	}
}

func TestCursorCapabilityMatrixMatchesBackendClaims(t *testing.T) {
	wired := map[Capability]bool{}
	for _, caps := range []providerCapabilities{
		(ompAgentBackend{}).Capabilities(),
		taskBackendForProvider(ProviderCursor).Capabilities(),
	} {
		for capability, ok := range backendCapabilityNames(caps) {
			wired[capability] = wired[capability] || ok
		}
	}
	if len(wired) == 0 {
		t.Fatal("no backend capabilities collected; the test would pass vacuously")
	}
	for capability, isWired := range wired {
		status := ProviderCapabilityStatus(ProviderCursor, capability)
		if isWired && status != CapabilitySupported {
			t.Errorf("a Cursor backend wires %s but the matrix says %q", capability, status)
		}
		if !isWired && status == CapabilitySupported {
			t.Errorf("no Cursor backend wires %s but the matrix claims supported", capability)
		}
	}
}

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
func TestGrokCapabilityMatrixIsExplicit(t *testing.T) {
	want := map[Capability]CapabilityStatus{
		CapabilityTask:    CapabilitySupported,
		CapabilityResume:  CapabilitySupported,
		CapabilitySession: CapabilitySupported,
		// Session is supported on Grok and experimental on Codex: ACP is
		// a public contract claudia already drives, so the two providers
		// are not interchangeable and the matrix must not flatten them.
		CapabilityRewind:           CapabilityUnsupported,
		CapabilityCost:             CapabilityUnsupported,
		CapabilityTmuxAttach:       CapabilityUnsupported,
		CapabilityTerminalLog:      CapabilityUnsupported,
		CapabilityPermissionMode:   CapabilityUnsupported,
		CapabilityToolRestrictions: CapabilityUnsupported,
		CapabilityImageInput:       CapabilityUnsupported,
		CapabilityWebSearch:        CapabilityUnsupported,
	}
	got := ProviderCapabilityMatrix(ProviderGrok)
	if len(got) != len(want) {
		t.Errorf("matrix has %d entries, want %d: %+v", len(got), len(want), got)
	}
	for capability, wantStatus := range want {
		if got[capability] != wantStatus {
			t.Errorf("grok %s = %q, want %q", capability, got[capability], wantStatus)
		}
		if wantStatus == CapabilitySupported {
			continue
		}
		// A gap without a rationale is an undocumented gap.
		if reason := ProviderCapabilityReason(ProviderGrok, capability); reason == "" {
			t.Errorf("grok %s is %q with no documented reason", capability, wantStatus)
		}
	}
}

// TestGrokCapabilityMatrixMatchesBackendClaims stops the internal and
// public views of Grok from drifting: the per-backend booleans say what
// is wired, the matrix says what is claimed, and either direction of
// mismatch is the fake parity the matrix exists to prevent.
//
// A capability counts as wired if *some* Grok backend routes it. The
// Codex version of this test can afford to assert per-backend only
// because no Codex backend claims Session; on Grok, Task and Session live
// in different backends, so demanding that each backend wire everything
// the provider claims would report grokTaskBackend for not implementing
// Session — a division of labour, not a defect.
func TestGrokCapabilityMatrixMatchesBackendClaims(t *testing.T) {
	wired := map[Capability]bool{}
	for _, caps := range []providerCapabilities{
		(grokTaskBackend{}).Capabilities(),
		(grokAgentBackend{}).Capabilities(),
	} {
		for capability, ok := range backendCapabilityNames(caps) {
			wired[capability] = wired[capability] || ok
		}
	}
	if len(wired) == 0 {
		t.Fatal("no backend capabilities collected; the test would pass vacuously")
	}
	for capability, isWired := range wired {
		status := ProviderCapabilityStatus(ProviderGrok, capability)
		if isWired && status != CapabilitySupported {
			t.Errorf("a Grok backend wires %s but the matrix says %q", capability, status)
		}
		if !isWired && status == CapabilitySupported {
			t.Errorf("no Grok backend wires %s but the matrix claims supported", capability)
		}
	}
}

// TestGrokToolRestrictionsClaimTracksTheArgvBuilder closes the blind spot
// the test above cannot cover. backendCapabilityNames has no field for
// tool_restrictions — no providerCapabilities boolean carries it — so the
// wired/claimed ratchet never visits that capability, and the published
// status is otherwise pinned only by a hand-written want map.
//
// That leaves one plausible edit fully green: flip the claim to
// supported, update the want map to match (the natural move when someone
// believes they have closed the gap), and leave grokTaskArgs untouched.
// Task.Run would still refuse — grokToolRestrictionRefusal fails closed
// on a supported claim — but callers would read `supported` from the
// published matrix and get a refusal at runtime. This asserts the
// implication directly against the argv builder, so the claim can only
// move once the translation actually exists.
func TestGrokToolRestrictionsClaimTracksTheArgvBuilder(t *testing.T) {
	if ProviderCapabilityStatus(ProviderGrok, CapabilityToolRestrictions) != CapabilitySupported {
		return // The gap is still declared; the refusal tests own this path.
	}
	argv := grokTaskArgs(taskRunRequest{
		Prompt:        "the prompt",
		DisallowTools: []string{"Bash"},
	})
	var restricted bool
	for _, arg := range argv {
		if arg == "--deny" || arg == "--disallowed-tools" {
			restricted = true
		}
	}
	if !restricted {
		t.Errorf("the matrix claims Grok tool_restrictions is supported, but grokTaskArgs "+
			"emits no --deny or --disallowed-tools: %v", argv)
	}
}

// TestGrokToolRestrictionsReasonNamesTheRealMechanism guards the honesty
// of the published rationale, which is the only thing a caller has to
// decide whether the gap is permanent.
//
// Grok is not Codex here. `codex exec` genuinely has no per-tool disallow
// flag; grok has two mechanisms — `--deny <RULE>` gating invocations and
// `--disallowed-tools <IDS>` removing tools from the toolset — and
// claiming otherwise would be a false statement about a third-party CLI
// that quietly converts a claudia to-do into a Grok limitation. The
// refusal stands on claudia not having proven its translation, so the
// reason must say that and not something flattering.
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

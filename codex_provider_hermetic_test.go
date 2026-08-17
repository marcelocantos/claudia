// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🎯T4.8 residual hermetics: capability claim + fixture inventory ratchet.
// Live residual stays under CLAUDIA_CODEX_LIVE=1 (TestCodexTaskRunSmoke).

func TestCodexProviderCapabilitiesClaimed(t *testing.T) {
	taskWant := providerCapabilities{Task: true, Resume: true}
	if got := (codexTaskBackend{}).Capabilities(); got != taskWant {
		t.Errorf("codexTaskBackend capabilities = %+v, want %+v", got, taskWant)
	}
	agentWant := providerCapabilities{Task: true, Resume: true, Session: true}
	if got := (codexAgentBackend{}).Capabilities(); got != agentWant {
		t.Errorf("codexAgentBackend capabilities = %+v, want %+v", got, agentWant)
	}
	if got := ProviderCapabilityStatus(ProviderCodex, CapabilitySession); got != CapabilitySupported {
		t.Errorf("codex session status = %q, want %q", got, CapabilitySupported)
	}
}

// TestCodexCapabilityMatrixIsExplicit pins the published status of every
// capability 🎯T4.6 names. The point is not that the values are these
// values; it is that each one is *stated*, so a Codex gap can never be
// discovered by a user at runtime instead of read off the matrix.
func TestCodexCapabilityMatrixIsExplicit(t *testing.T) {
	want := map[Capability]CapabilityStatus{
		CapabilityTask:             CapabilitySupported,
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
		// 🎯T24: sandbox_policy is the one capability Codex has and
		// Claude does not — SandboxMode/ApprovalPolicy are `codex exec`
		// flags, and every other provider now refuses them instead of
		// dropping them.
		CapabilitySandboxPolicy: CapabilitySupported,
		CapabilityExtraArgs:     CapabilityUnsupported,
	}
	got := ProviderCapabilityMatrix(ProviderCodex)
	if len(got) != len(want) {
		t.Errorf("matrix has %d entries, want %d: %+v", len(got), len(want), got)
	}
	for capability, wantStatus := range want {
		if got[capability] != wantStatus {
			t.Errorf("codex %s = %q, want %q", capability, got[capability], wantStatus)
		}
		if wantStatus == CapabilitySupported {
			continue
		}
		// A gap without a rationale is an undocumented gap.
		if reason := ProviderCapabilityReason(ProviderCodex, capability); reason == "" {
			t.Errorf("codex %s is %q with no documented reason", capability, wantStatus)
		}
	}
}

// TestCodexCapabilityGapsVersusClaude is the head-to-head 🎯T4.6 asks
// for: everything Claude supports that Codex does not must appear as a
// declared gap, and calling into it must fail with a typed error rather
// than inheriting Claude's answer.
func TestCodexCapabilityGapsVersusClaude(t *testing.T) {
	wantGaps := []Capability{
		CapabilityRewind,
		CapabilityCost,
		CapabilityTmuxAttach,
		CapabilityTerminalLog,
		CapabilityPermissionMode,
		CapabilityToolRestrictions,
		CapabilityWebSearch,
		CapabilityExtraArgs,
	}
	for _, capability := range wantGaps {
		if ProviderCapabilityStatus(ProviderClaude, capability) != CapabilitySupported {
			t.Errorf("claude %s is not supported; the gap list is stale", capability)
		}
		err := CheckCapability(ProviderCodex, capability)
		if err == nil {
			t.Errorf("CheckCapability(codex, %s) = nil, want a typed capability error", capability)
			continue
		}
		var capErr *CapabilityError
		if !errors.As(err, &capErr) {
			t.Errorf("CheckCapability(codex, %s) = %T, want *CapabilityError", capability, err)
			continue
		}
		if capErr.Provider != ProviderCodex || capErr.Capability != capability {
			t.Errorf("CapabilityError = %+v, want codex/%s", capErr, capability)
		}
		if capErr.Status == CapabilitySupported {
			t.Errorf("codex %s error reports status %q", capability, capErr.Status)
		}
	}
	// CapabilityImageInput is a shared gap, not a Codex-specific one:
	// claudia plumbs images for nobody. Asserting it here would quietly
	// turn a whole-library gap into a Codex indictment.
	if ProviderCapabilityStatus(ProviderClaude, CapabilityImageInput) == CapabilitySupported {
		t.Error("claude image_input became supported; move it into wantGaps")
	}
	if gaps := capabilityGaps(ProviderCodex); len(gaps) != len(wantGaps)+1 {
		t.Errorf("codex gaps = %v, want the %d declared gaps plus image_input", gaps, len(wantGaps))
	}
}

// TestCodexCapabilityMatrixMatchesBackendClaims stops the two views from
// drifting: the internal per-backend booleans say what is wired, the
// public matrix says what is claimed, and a wired-but-unclaimed (or
// claimed-but-unwired) capability is exactly the fake parity 🎯T4.6
// exists to prevent.
func TestCodexCapabilityMatrixMatchesBackendClaims(t *testing.T) {
	task := (codexTaskBackend{}).Capabilities()
	agent := (codexAgentBackend{}).Capabilities()
	wired := providerCapabilities{
		Task:    task.Task || agent.Task,
		Session: task.Session || agent.Session,
		Resume:  task.Resume || agent.Resume,
		Rewind:  task.Rewind || agent.Rewind,
		Cost:    task.Cost || agent.Cost,
	}
	for capability, on := range backendCapabilityNames(wired) {
		status := ProviderCapabilityStatus(ProviderCodex, capability)
		if on && status != CapabilitySupported {
			t.Errorf("a Codex backend wires %s but the matrix says %q", capability, status)
		}
		if !on && status == CapabilitySupported {
			t.Errorf("no Codex backend wires %s but the matrix claims supported", capability)
		}
	}
}

func TestCodexOracleFixturesPresent(t *testing.T) {
	// Ratchet: every path named by docs/codex-provider-oracle-map.md must exist.
	// Prevents "oracle map says hermetic" while fixtures drift off disk.
	required := []string{
		"testdata/codex/exec/success.jsonl",
		"testdata/codex/exec/failure.jsonl",
		"testdata/codex/exec/error.jsonl",
		"testdata/codex/exec/malformed.jsonl",
		"testdata/codex/app-server/success.jsonl",
		"testdata/codex/app-server/failure.jsonl",
		"testdata/codex/app-server/interrupted.jsonl",
		"testdata/codex/app-server/unsupported-capability.jsonl",
		"testdata/codex/app-server/thread-start.jsonl",
		"testdata/codex/app-server/live-turn.jsonl",
		"testdata/codex/app-server/lifecycle.jsonl",
		"docs/codex-provider-oracle-map.md",
	}
	for _, path := range required {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("required oracle asset missing: %s (%v)", path, err)
			continue
		}
		if info.IsDir() {
			t.Errorf("required oracle asset is a directory: %s", path)
		}
		if info.Size() == 0 {
			t.Errorf("required oracle asset is empty: %s", path)
		}
	}
}

func TestCodexOracleMapNamesLiveAsSmokeOnly(t *testing.T) {
	// Docs invariant: live Codex must never be the sole retirement evidence.
	data, err := os.ReadFile(filepath.Join("docs", "codex-provider-oracle-map.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, needle := range []string{
		"smoke/regression only",
		"never sole retirement evidence",
		"CLAUDIA_CODEX_LIVE",
		"TestCodexTaskSuccessOracleRejectsFaults",
		"TestFakeCodexAppServerLifecycle",
		"TestCodexProviderDoesNotReadPrivateStorage",
		// 🎯T4.6 capability oracles: the map must name the tests that
		// actually hold the gaps open, or "hermetic sealed" is a claim
		// with nothing behind it.
		"TestCodexCapabilityMatrixIsExplicit",
		"TestCodexCapabilityGapsVersusClaude",
		"TestCodexCapabilityMatrixMatchesBackendClaims",
		"TestCodexTaskToolRestrictionsFailClosed",
		"TestCodexTaskWithoutRestrictionsIsNotBlocked",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("codex-provider-oracle-map.md missing required seal token %q", needle)
		}
	}
}

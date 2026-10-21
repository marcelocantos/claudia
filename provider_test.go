// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/gowalk"
)

func TestProviderCapabilityMatrixIsTotal(t *testing.T) {
	providers := []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderBedrock, ProviderOllama, ProviderCursor}
	for _, provider := range providers {
		claims, ok := providerCapabilityClaims[provider]
		if !ok {
			t.Errorf("provider %q has no capability claims at all", provider)
			continue
		}
		for _, capability := range reportedCapabilities() {
			claim, ok := claims[capability]
			if !ok {
				t.Errorf("provider %q makes no claim about %s", provider, capability)
				continue
			}
			switch claim.status {
			case CapabilitySupported:
			case CapabilityUnsupported, CapabilityExperimental:
				if claim.reason == "" {
					t.Errorf("provider %q %s is %q with no reason", provider, capability, claim.status)
				}
			default:
				t.Errorf("provider %q %s has unknown status %q", provider, capability, claim.status)
			}
		}
		for capability := range claims {
			if !slices.Contains(reportedCapabilities(), capability) {
				t.Errorf("provider %q claims unreported capability %s", provider, capability)
			}
		}
	}
}

// TestSteerableIsGenuinelyComputed: Steerable's verdict changes when the
// underlying capability claim changes (and only when it changes) — it is
// not a jevons-supplied answer being echoed back. Mutate the claim table
// directly, the way TestProviderCapabilityMatrixIsTotal and friends
// already do for other capabilities, to prove the computation lives here.
func TestSteerableIsGenuinelyComputed(t *testing.T) {
	ok, reason := Steerable(ProviderClaude)
	if !ok || reason != "" {
		t.Fatalf("Steerable(claude) = (%v, %q), want (true, \"\")", ok, reason)
	}

	// Cursor seats run in the OMP sidecar, which binds the offered tools
	// itself; the cursor-agent ACP limit (claudia T118) no longer applies.
	ok, reason = Steerable(ProviderCursor)
	if !ok || reason != "" {
		t.Fatalf("Steerable(cursor) = (%v, %q), want (true, \"\") — cursor seats run in the sidecar", ok, reason)
	}

	// Mutate the claim table (restored after) and confirm the verdict
	// tracks it — the function reads the table, it does not hardcode an
	// answer per provider name.
	orig := providerCapabilityClaims[ProviderCursor][CapabilityMCPTools]
	defer func() { providerCapabilityClaims[ProviderCursor][CapabilityMCPTools] = orig }()

	providerCapabilityClaims[ProviderCursor][CapabilityMCPTools] = capabilityClaim{status: CapabilitySupported}
	ok, reason = Steerable(ProviderCursor)
	if !ok || reason != "" {
		t.Fatalf("after flipping the claim, Steerable(cursor) = (%v, %q), want (true, \"\")", ok, reason)
	}

	providerCapabilityClaims[ProviderCursor][CapabilityMCPTools] = capabilityClaim{status: CapabilityUnsupported, reason: "mutated for test"}
	ok, reason = Steerable(ProviderCursor)
	if ok || reason != "mutated for test" {
		t.Fatalf("after mutating the claim, Steerable(cursor) = (%v, %q), want (false, %q)", ok, reason, "mutated for test")
	}
}

// TestSteerableCoversEveryProvider: every provider the public Provider
// constants name gets a verdict, not a panic or a silent empty string
// masquerading as "ok".
func TestSteerableCoversEveryProvider(t *testing.T) {
	for _, p := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderBedrock, ProviderOllama, ProviderCursor} {
		ok, reason := Steerable(p)
		if !ok && reason == "" {
			t.Errorf("Steerable(%q) = (false, \"\"); an unsupported verdict must carry a reason", p)
		}
	}
}

// TestCheckCapabilityFailsClosed: an unknown provider or an unclaimed
// capability must report unsupported, never inherit Claude's answer.
func TestCheckCapabilityFailsClosed(t *testing.T) {
	// Task has no OMP replacement (T192). Empty provider is Claude.
	for _, p := range []Provider{ProviderClaude, ""} {
		err := CheckCapability(p, CapabilityTask)
		var capErr *CapabilityError
		if !errors.As(err, &capErr) || capErr.Status != CapabilityUnsupported {
			t.Errorf("CheckCapability(%q, task) = %v, want unsupported", p, err)
		}
	}
	for _, tc := range []struct {
		provider   Provider
		capability Capability
	}{
		{"nonesuch", CapabilityTask},
		{ProviderCodex, "teleportation"},
	} {
		err := CheckCapability(tc.provider, tc.capability)
		var capErr *CapabilityError
		if !errors.As(err, &capErr) {
			t.Errorf("CheckCapability(%q, %s) = %T %v, want *CapabilityError",
				tc.provider, tc.capability, err, err)
			continue
		}
		if capErr.Status != CapabilityUnsupported {
			t.Errorf("CheckCapability(%q, %s) status = %q, want unsupported",
				tc.provider, tc.capability, capErr.Status)
		}
	}
}

func TestCapabilityErrorMessage(t *testing.T) {
	err := unsupportedCapability(ProviderCodex, "rewind", "requires public fork API")
	msg := err.Error()
	for _, want := range []string{"codex", "rewind", "unsupported", "requires public fork API"} {
		if !strings.Contains(msg, want) {
			t.Errorf("CapabilityError message %q does not contain %q", msg, want)
		}
	}
}

func TestExperimentalCapabilityErrorMessage(t *testing.T) {
	err := experimentalCapability(ProviderCodex, "session", "app-server turn contract is unproven")
	if err.Status != CapabilityExperimental {
		t.Fatalf("Status = %q, want %q", err.Status, CapabilityExperimental)
	}
	msg := err.Error()
	for _, want := range []string{"codex", "session", "experimental", "app-server turn contract is unproven"} {
		if !strings.Contains(msg, want) {
			t.Errorf("CapabilityError message %q does not contain %q", msg, want)
		}
	}
}

func TestCodexAppServerFixturesAreValidJSONL(t *testing.T) {
	cases := []struct {
		path       string
		wantToken  string
		wantMethod string
	}{
		{"testdata/codex/app-server/thread-start.jsonl", "thr_redacted", "thread/started"},
		{"testdata/codex/app-server/success.jsonl", "turn_success", "turn/completed"},
		{"testdata/codex/app-server/failure.jsonl", "model failed", "turn/completed"},
		{"testdata/codex/app-server/interrupted.jsonl", "turn_interrupted", "turn/completed"},
		{"testdata/codex/app-server/unsupported-capability.jsonl", "experimentalApi", ""},
		{"testdata/codex/app-server/live-turn.jsonl", "T4.4-ok", "turn/completed"},
		{"testdata/codex/app-server/lifecycle.jsonl", "thr_fork", "turn/completed"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var sawMethod bool
			var sawToken bool
			for _, line := range readFixtureLines(t, tc.path) {
				var msg map[string]any
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					t.Fatalf("invalid JSONL line %q: %v", line, err)
				}
				if method, _ := msg["method"].(string); method == tc.wantMethod {
					sawMethod = true
				}
				if strings.Contains(line, tc.wantToken) {
					sawToken = true
				}
			}
			if tc.wantMethod != "" && !sawMethod {
				t.Errorf("%s did not contain method %s", tc.path, tc.wantMethod)
			}
			if !sawToken {
				t.Errorf("%s did not contain token %s", tc.path, tc.wantToken)
			}
		})
	}
}

func TestGrokProviderDoesNotReadPrivateStorage(t *testing.T) {
	// Session control plane must use public CLI/ACP contracts, not private
	// session files. Docs may mention the path; production .go must not.
	forbidden := []string{
		".grok/sessions",
		"~/.grok/sessions",
	}
	if err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// gowalk.IgnoredDir keeps this walk to the files the toolchain
			// compiles, so a scratch copy of a provider file cannot fail it
			// on a token it only quotes (T99).
			if path != "." && (gowalk.IgnoredDir(entry.Name()) || path == "docs") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range forbidden {
			if strings.Contains(string(data), token) {
				t.Errorf("%s contains private Grok storage token %q", path, token)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexProviderDoesNotReadPrivateStorage(t *testing.T) {
	forbidden := []string{
		".codex/sessions",
		".codex/history",
		".codex/threads",
		".codex/rollouts",
		"rollout_path",
	}
	if err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// gowalk.IgnoredDir keeps this walk to the files the toolchain
			// compiles, so a scratch copy of a provider file cannot fail it
			// on a token it only quotes (T99).
			if path != "." && (gowalk.IgnoredDir(entry.Name()) || path == "docs") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range forbidden {
			if strings.Contains(string(data), token) {
				t.Errorf("%s contains private Codex storage token %q", path, token)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

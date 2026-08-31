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

func TestCursorCapabilityMatrixIsExplicit(t *testing.T) {
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
		CapabilitySandboxPolicy:    CapabilityUnsupported,
		CapabilityExtraArgs:        CapabilityUnsupported,
		CapabilityModelSwitch:      CapabilitySupported,
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
		(cursorAgentBackend{}).Capabilities(),
		(cursorTaskBackend{}).Capabilities(),
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

func TestResolveCursorBin(t *testing.T) {
	fake := "/tmp/fake-agent"
	errNotFound := errors.New("not found")

	statExisting := func(paths ...string) func(string) (os.FileInfo, error) {
		exists := make(map[string]bool, len(paths))
		for _, p := range paths {
			exists[p] = true
		}
		return func(path string) (os.FileInfo, error) {
			if exists[path] {
				return nil, nil
			}
			return nil, errNotFound
		}
	}

	got, err := resolveCursorBinFrom(
		func(string) string { return fake },
		func(string) (string, error) { return "", errNotFound },
		statExisting(fake),
		nil,
	)
	if err != nil {
		t.Fatalf("CURSOR_BIN absolute: %v", err)
	}
	if got != fake {
		t.Fatalf("got %q, want %q", got, fake)
	}

	got, err = resolveCursorBinFrom(
		func(string) string { return "" },
		func(name string) (string, error) {
			if name == cursorBinAlias {
				return fake, nil
			}
			return "", errNotFound
		},
		statExisting(),
		nil,
	)
	if err != nil {
		t.Fatalf("PATH cursor-agent: %v", err)
	}
	if got != fake {
		t.Fatalf("PATH cursor-agent got %q, want %q", got, fake)
	}

	got, err = resolveCursorBinFrom(
		func(string) string { return "" },
		func(name string) (string, error) {
			if name == cursorBinName {
				return fake, nil
			}
			return "", errNotFound
		},
		statExisting(),
		nil,
	)
	if err != nil {
		t.Fatalf("PATH agent: %v", err)
	}
	if got != fake {
		t.Fatalf("PATH agent got %q, want %q", got, fake)
	}

	local := "/tmp/local-cursor-agent"
	got, err = resolveCursorBinFrom(
		func(string) string { return "" },
		func(name string) (string, error) {
			if name == cursorBinName {
				return "/home/u/.grok/bin/agent", nil
			}
			return "", errNotFound
		},
		statExisting(local),
		[]string{local},
	)
	if err != nil {
		t.Fatalf("skip grok agent: %v", err)
	}
	if got != local {
		t.Fatalf("skip grok agent got %q, want %q", got, local)
	}

	_, err = resolveCursorBinFrom(
		func(string) string { return "" },
		func(name string) (string, error) {
			if name == cursorBinName || name == cursorBinAlias {
				return "/home/u/.grok/bin/agent", nil
			}
			return "", errNotFound
		},
		statExisting(),
		nil,
	)
	if err == nil {
		t.Fatal("expected error when only grok agent is on PATH")
	}

	_, err = resolveCursorBinFrom(
		func(string) string { return "" },
		func(string) (string, error) { return "", errNotFound },
		statExisting(),
		[]string{"/no/such/agent"},
	)
	if err == nil {
		t.Fatal("expected error when agent is absent")
	}
	if !strings.Contains(err.Error(), cursorBinEnv) {
		t.Errorf("error %q does not mention %s", err.Error(), cursorBinEnv)
	}
}

func TestCursorBinCandidatesIncludeLocalBin(t *testing.T) {
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "bin", cursorBinName)
	for _, candidate := range cursorBinCandidates() {
		if candidate == want {
			return
		}
	}
	t.Fatalf("cursorBinCandidates() does not include %s", want)
}

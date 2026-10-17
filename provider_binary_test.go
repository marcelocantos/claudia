// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ChatGPT 26.930 (Colossus, 2026-10-04) moved its bundled Codex CLI from
// Resources/codex to Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex.
// With only the old path listed, a supervisord broker whose PATH has no
// codex found nothing, and every --pick remaining that chose codex died at
// seat start. Removing the new candidate turns this red.
func TestResolveCodexBinFindsChatGPTCodexCLIBundle(t *testing.T) {
	const want = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
	errNotFound := errors.New("not found")
	got, err := resolveCodexBinFrom(
		func(string) string { return "" },
		func(string) (string, error) { return "", errNotFound },
		func(p string) (os.FileInfo, error) {
			if p == want {
				return nil, nil
			}
			return nil, errNotFound
		},
		codexBinCandidates(),
	)
	if err != nil || got != want {
		t.Fatalf("resolveCodexBinFrom = %q, %v; want %q", got, err, want)
	}
}

// The user-local dirs a login shell puts on PATH but a launchd or
// supervisord daemon does not: npm-global, bun, volta, ~/.local/bin.
func TestCodexBinCandidatesCoverUserLocalInstallDirs(t *testing.T) {
	home, _ := os.UserHomeDir()
	have := map[string]bool{}
	for _, c := range codexBinCandidates() {
		have[c] = true
	}
	for _, dir := range []string{".local/bin", ".npm-global/bin", ".bun/bin", ".volta/bin"} {
		if p := filepath.Join(home, dir, "codex"); !have[p] {
			t.Errorf("codexBinCandidates() lacks %s", p)
		}
	}
	for _, p := range []string{"/opt/homebrew/bin/codex", "/usr/local/bin/codex", chatGPTBundledCodex} {
		if !have[p] {
			t.Errorf("codexBinCandidates() lacks %s", p)
		}
	}
}

// Seats for Cursor and Grok run in the plan sidecar, and Bedrock/Ollama
// are API backends: a missing CLI must not exclude them from a seat pick.
func TestProviderBinaryErrorSurfaces(t *testing.T) {
	for _, p := range []Provider{ProviderCursor, ProviderGrok} {
		if err := ProviderBinaryError(p, true); err != nil {
			t.Errorf("%s session: %v, want nil (sidecar seat runs no local CLI)", p, err)
		}
	}
	for _, p := range []Provider{ProviderBedrock, ProviderOllama} {
		for _, session := range []bool{true, false} {
			if err := ProviderBinaryError(p, session); err != nil {
				t.Errorf("%s session=%v: %v, want nil (API backend)", p, session, err)
			}
		}
	}
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexBinEnv, bin)
	if err := ProviderBinaryError(ProviderCodex, true); err != nil {
		t.Errorf("codex with %s=%s: %v", codexBinEnv, bin, err)
	}
}

// The Task surface is the production caller that used to resolve a
// subscription CLI. It must name the removal and must not look up or
// start claude, codex, grok, or agent. A session check still does not.
func TestTaskSurfaceBinaryErrorDoesNotResolveCLI(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "marker")
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	for _, name := range []string{"claude", "codex", "grok", "agent", "cursor-agent"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CLAUDE_BIN", filepath.Join(binDir, "claude"))
	t.Setenv("CODEX_BIN", filepath.Join(binDir, "codex"))
	t.Setenv("GROK_BIN", filepath.Join(binDir, "grok"))
	t.Setenv("CURSOR_BIN", filepath.Join(binDir, "agent"))
	t.Setenv("TMPDIR", binDir)

	for _, p := range []Provider{"", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor, "anthropic", "openai-codex", "xai-oauth"} {
		err := ProviderBinaryError(p, false)
		if !errors.Is(err, ErrTaskCLIRemoved) {
			t.Fatalf("%q task: %v", p, err)
		}
	}
	for _, p := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor} {
		if err := ProviderBinaryError(p, true); err != nil {
			t.Fatalf("%s session: %v, want nil", p, err)
		}
	}
	for _, p := range []Provider{ProviderBedrock, ProviderOllama} {
		if err := ProviderBinaryError(p, false); err != nil {
			t.Fatalf("%s task: %v, want nil", p, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("subscription CLI was resolved or started: %v", err)
	}
}

func TestExcludeUnlaunchableMarksRowNotAdmitted(t *testing.T) {
	pct := 97.0
	usage := []PlanUsage{{Provider: ProviderCodex, Status: PlanUsageAvailable,
		Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &pct}}}}
	snap := ProjectFleetUsage(usage, time.Time{}, time.Now(), nil)
	snap = ExcludeUnlaunchable(snap, map[Provider]string{
		ProviderCodex: "codex executable not found in PATH or known install dirs (set CODEX_BIN to override)",
	})
	for _, row := range snap.Providers {
		if row.Provider != ProviderCodex {
			continue
		}
		if row.Admit {
			t.Fatal("codex with no binary is still admitted")
		}
		if !strings.HasPrefix(row.Reason, FleetReasonBinaryNotFound+": codex executable not found") {
			t.Fatalf("reason = %q", row.Reason)
		}
		if row.RemainingPercent == nil || *row.RemainingPercent != 97 {
			t.Fatalf("remaining percent must stay reported, got %v", row.RemainingPercent)
		}
		return
	}
	t.Fatal("no codex row")
}

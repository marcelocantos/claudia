// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🎯T4.8 residual hermetics: capability claim + fixture inventory ratchet.
// Live residual stays under CLAUDIA_CODEX_LIVE=1 (TestCodexTaskRunSmoke).

func TestCodexProviderCapabilitiesClaimed(t *testing.T) {
	want := providerCapabilities{
		Task:   true,
		Resume: true,
		// Session remains experimental fail-closed on production Start until
		// the app-server live contract (T4.4) lands. Rewind/cost/tmux/log are
		// unsupported on the public Codex surface we bind today.
	}
	if got := (codexTaskBackend{}).Capabilities(); got != want {
		t.Errorf("codexTaskBackend capabilities = %+v, want %+v", got, want)
	}
	if got := (codexAgentBackend{}).Capabilities(); got != want {
		t.Errorf("codexAgentBackend capabilities = %+v, want %+v", got, want)
	}
	if want.Session || want.Rewind || want.Cost || want.TmuxAttach || want.TerminalBytes {
		t.Fatal("codex claim must not advertise Session/Rewind/Cost/Tmux/TerminalBytes")
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
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("codex-provider-oracle-map.md missing required seal token %q", needle)
		}
	}
}

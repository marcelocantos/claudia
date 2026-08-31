// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🎯T598. A Codex work seat must be able to write its gate record outside
// the workspace and bind a loopback port. thread/start cannot express
// either: its `sandbox` field is a unit variant, and a map there is
// refused with "invalid type: map, expected unit". The dimensions live in
// CODEX_HOME/config.toml, which claudia already owns per session.
func TestCodexSandboxTOMLCarriesWhatThreadStartCannot(t *testing.T) {
	got := codexSandboxTOML(codexSandboxTuning{
		WritableRoots: []string{"/Users/x/.jevons/gates", "/tmp/scratch"},
		NetworkAccess: true,
	})
	for _, want := range []string{
		"[sandbox_workspace_write]",
		`writable_roots = ["/Users/x/.jevons/gates", "/tmp/scratch"]`,
		"network_access = true",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stanza missing %q:\n%s", want, got)
		}
	}
}

func TestCodexSandboxTOMLIsSilentWhenNothingIsAsked(t *testing.T) {
	// An empty tuning must leave Codex's own defaults alone rather than
	// writing an empty stanza that overrides the user's config.
	if got := codexSandboxTOML(codexSandboxTuning{}); got != "" {
		t.Fatalf("empty tuning wrote %q", got)
	}
	// Blank roots are not roots.
	if got := codexSandboxTOML(codexSandboxTuning{WritableRoots: []string{"", "  "}}); got != "" {
		t.Fatalf("blank roots wrote %q", got)
	}
}

func TestExclusiveHomeWritesTheSandboxStanzaBesideMCP(t *testing.T) {
	dest := t.TempDir()
	if err := writeExclusiveCodexHome(dest, nil, codexSandboxTuning{
		WritableRoots: []string{"/tmp/t598-gates"},
		NetworkAccess: true,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "/tmp/t598-gates") || !strings.Contains(s, "network_access = true") {
		t.Fatalf("config.toml lacks the sandbox stanza:\n%s", s)
	}
	// The MCP header it has always written must survive.
	if !strings.Contains(s, "claudia session MCP") {
		t.Fatalf("sandbox stanza replaced the MCP config:\n%s", s)
	}
}

// A seat asking only for sandbox tuning still needs a private CODEX_HOME:
// without one there is no config.toml to write and the request would be
// dropped in silence.
func TestSandboxTuningDemandsAPrivateHome(t *testing.T) {
	if needsSessionMCPMaterialization(Config{}) {
		t.Fatal("a bare config should not demand a private home")
	}
	for _, cfg := range []Config{
		{SandboxWritableRoots: []string{"/tmp/gates"}},
		{SandboxNetworkAccess: true},
	} {
		if !needsSessionMCPMaterialization(cfg) {
			t.Fatalf("sandbox tuning %+v did not demand a private home", cfg)
		}
	}
}

// 🎯T598: asking is not getting. thread/start accepts an unknown
// sandboxPolicy and discards it silently, so the echoed sandbox is the
// only evidence of what a seat actually runs in.
func TestEffectiveSandboxIsReadFromTheThreadResult(t *testing.T) {
	line := []byte(`{"id":2,"result":{"thread":{"id":"t","sandbox":` +
		`{"type":"workspaceWrite","writableRoots":["/tmp/gates"],"networkAccess":true}}}}`)
	got := parseEffectiveSandbox(line)
	if got.Type != "workspaceWrite" || !got.NetworkAccess {
		t.Fatalf("parsed %+v", got)
	}
	if len(got.WritableRoots) != 1 || got.WritableRoots[0] != "/tmp/gates" {
		t.Fatalf("writable roots %+v", got.WritableRoots)
	}
	// The shape the app-server actually returns for an unwidened seat —
	// the one that produced the reported breakage.
	bare := parseEffectiveSandbox([]byte(
		`{"result":{"thread":{"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false}}}}`))
	if bare.NetworkAccess || len(bare.WritableRoots) != 0 {
		t.Fatalf("bare sandbox parsed as widened: %+v", bare)
	}
	// A result with no sandbox at all must not look like a denial.
	if none := parseEffectiveSandbox([]byte(`{"result":{"thread":{"id":"t"}}}`)); none.Type != "" {
		t.Fatalf("absent sandbox parsed as %+v", none)
	}
}

func TestSandboxModeMapsToTheEchoedType(t *testing.T) {
	for mode, want := range map[string]string{
		"workspace-write":    "workspaceWrite",
		"read-only":          "readOnly",
		"danger-full-access": "dangerFullAccess",
		"not-a-mode":         "",
	} {
		if got := codexSandboxTypeFor(mode); got != want {
			t.Fatalf("%s → %q, want %q", mode, got, want)
		}
	}
}

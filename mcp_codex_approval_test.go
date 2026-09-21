// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Codex 0.155 rejects an MCP tool call under approvalPolicy "never" unless
// the server block says its tools need no approval. Every Codex seat lost
// every MCP tool on 2026-09-21 when ChatGPT.app updated its bundled CLI; an
// A/B against that binary showed the one key below is the difference between
// "MCP tool call requires approval, but approval policy is never" and a
// served call.
func TestCodexSeatMCPServersNeedNoToolApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dest := filepath.Join(t.TempDir(), "codex-home")
	servers := []MCPServer{
		{Name: "jevonsmcp", URL: "http://127.0.0.1:13705/mcp"},
		{Name: "mnemo", URL: "http://127.0.0.1:19419/mcp"},
	}
	if err := writeExclusiveCodexHome(dest, servers, codexSandboxTuning{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	want := `default_tools_approval_mode = "approve"`
	if got := strings.Count(body, want); got != len(servers) {
		t.Fatalf("%d of %d server blocks carry %s:\n%s", got, len(servers), want, body)
	}
}

// A home written before the key existed is upgraded on the next start, not
// left matching the old shape forever.
func TestCodexMCPBlockWithoutApprovalModeIsRewritten(t *testing.T) {
	srv := MCPServer{Name: "jevonsmcp", URL: "http://127.0.0.1:13705/mcp"}
	old := "[mcp_servers.jevonsmcp]\nurl = \"http://127.0.0.1:13705/mcp\"\nenabled = true\n"

	next, changed := mergeTOMLHTTPServer(old, srv)
	if !changed || !strings.Contains(next, `default_tools_approval_mode = "approve"`) {
		t.Fatalf("pre-approval block left as it was (changed=%v):\n%s", changed, next)
	}
	if again, changedAgain := mergeTOMLHTTPServer(next, srv); changedAgain || again != next {
		t.Fatalf("an up-to-date block was rewritten:\n%s", again)
	}
}

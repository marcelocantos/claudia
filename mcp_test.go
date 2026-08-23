// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMCPReadsClaudeUserMapAndProjectOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	work := "/work/repo"
	doc := map[string]any{
		"mcpServers": map[string]any{
			"mnemo": map[string]any{"type": "http", "url": "http://127.0.0.1:7700/mcp"},
			"old":   map[string]any{"type": "http", "url": "http://127.0.0.1:1/mcp"},
		},
		"projects": map[string]any{
			work: map[string]any{
				"mcpServers": map[string]any{
					"old": map[string]any{"type": "http", "url": "http://127.0.0.1:2/mcp"},
				},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	inv, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: path})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Source != path {
		t.Fatalf("Source = %q", inv.Source)
	}
	byName := mcpByName(inv.Servers)
	if byName["mnemo"].URL != "http://127.0.0.1:7700/mcp" {
		t.Fatalf("user mnemo = %+v", byName["mnemo"])
	}
	if !hasProvider(byName["mnemo"].Providers, ProviderClaude) || hasProvider(byName["mnemo"].Providers, ProviderCodex) {
		t.Fatalf("claude-only mnemo providers = %v", byName["mnemo"].Providers)
	}
	if byName["old"].URL != "http://127.0.0.1:1/mcp" {
		t.Fatalf("user old = %+v", byName["old"])
	}

	over, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: path, WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	byName = mcpByName(over.Servers)
	if byName["old"].URL != "http://127.0.0.1:2/mcp" {
		t.Fatalf("project overlay old = %+v", byName["old"])
	}
	if byName["mnemo"].URL != "http://127.0.0.1:7700/mcp" {
		t.Fatalf("mnemo dropped by overlay: %+v", byName["mnemo"])
	}
}

func TestLoadMCPPerProviderOrigins(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude.json")
	grok := filepath.Join(dir, "grok.toml")
	codex := filepath.Join(dir, "codex.toml")
	doc := map[string]any{
		"mcpServers": map[string]any{
			"mnemo":     map[string]any{"type": "http", "url": "http://127.0.0.1:7700/mcp"},
			"atlassian": map[string]any{"type": "http", "url": "https://mcp.atlassian.com/v1/mcp/authv2"},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grok, []byte(`
[mcp_servers.mnemo]
url = "http://127.0.0.1:7700/mcp"
enabled = true

[mcp_servers.orthograph]
url = "http://127.0.0.1:13720/mcp"
enabled = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte(`
[mcp_servers.computer-use]
command = "./SkyComputerUseClient"
args = ["mcp"]

[mcp_servers.mnemo]
url = "http://127.0.0.1:7700/mcp"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	inv, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: claude, GrokTOML: grok, CodexTOML: codex})
	if err != nil {
		t.Fatal(err)
	}
	byName := mcpByName(inv.Servers)
	if !hasProvider(byName["atlassian"].Providers, ProviderClaude) || hasProvider(byName["atlassian"].Providers, ProviderGrok) {
		t.Fatalf("atlassian providers = %v", byName["atlassian"].Providers)
	}
	if !hasProvider(byName["orthograph"].Providers, ProviderGrok) || hasProvider(byName["orthograph"].Providers, ProviderClaude) {
		t.Fatalf("orthograph providers = %v", byName["orthograph"].Providers)
	}
	cu := byName["computer-use"]
	if cu.Command == "" || cu.URL != "" || !hasProvider(cu.Providers, ProviderCodex) || hasProvider(cu.Providers, ProviderClaude) {
		t.Fatalf("computer-use = %+v", cu)
	}
	mnemo := byName["mnemo"]
	for _, p := range []Provider{ProviderClaude, ProviderGrok, ProviderCodex} {
		if !hasProvider(mnemo.Providers, p) {
			t.Fatalf("mnemo missing %s: %v", p, mnemo.Providers)
		}
	}

	claudeOnly := (&MCPInventory{Servers: inv.Servers}).ForProvider(ProviderClaude)
	claudeNames := mcpByName(claudeOnly)
	if _, ok := claudeNames["computer-use"]; ok {
		t.Fatal("ForProvider(Claude) leaked Codex-only computer-use")
	}
	if _, ok := claudeNames["atlassian"]; !ok {
		t.Fatal("ForProvider(Claude) dropped atlassian")
	}
	if _, ok := claudeNames["mnemo"]; !ok {
		t.Fatal("ForProvider(Claude) dropped shared mnemo")
	}

	// Caller-appended servers (empty Providers) are valid for every backend.
	inv.Servers = append(inv.Servers, MCPServer{Name: "jevonsmcp", URL: "http://127.0.0.1:13705/mcp"})
	if mcpByName(inv.ForProvider(ProviderCodex))["jevonsmcp"].URL == "" {
		t.Fatal("empty Providers should be included for Codex")
	}
}

func TestConfigMCPServersReachClaudeArgvAndGrokACP(t *testing.T) {
	req := agentStartRequest{
		WorkDir: "/work/t40",
		Config: Config{
			MCPServers: []MCPServer{{Name: "mnemo", Type: "http", URL: "http://127.0.0.1:7700/mcp"}},
		},
	}
	args := claudeAgentArgs(req)
	if !argvHolds(args, "--mcp-config") || !argvHolds(args, claudeMCPConfigArg(req)) {
		t.Fatalf("claude argv missing session mcp sentinel: %v", args)
	}
	if claudeMCPConfigArg(req) != "session:mcpServers" {
		t.Fatalf("claudeMCPConfigArg = %q", claudeMCPConfigArg(req))
	}
	acp := resolveACPMCPServers(req.Config)
	if len(acp) != 1 {
		t.Fatalf("acp = %#v", acp)
	}
	m := acp[0].(map[string]any)
	if m["name"] != "mnemo" || m["url"] != "http://127.0.0.1:7700/mcp" {
		t.Fatalf("acp entry = %#v", m)
	}
}

func TestPrepareClaudeMCPConfigIsInlineOrTemp(t *testing.T) {
	arg, cleanup, err := prepareClaudeMCPConfig(Config{
		MCPServers: []MCPServer{{Name: "mnemo", URL: "http://127.0.0.1:7700/mcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	if !strings.Contains(arg, "mnemo") || !strings.Contains(arg, "http://127.0.0.1:7700/mcp") {
		t.Fatalf("inline/temp mcp = %q", arg)
	}
}

func TestMCPAuthRoundTripAndACP(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude.json")
	doc := map[string]any{
		"mcpServers": map[string]any{
			"private": map[string]any{
				"type": "http",
				"url":  "https://mcp.example.com/mcp",
				"headers": map[string]any{
					"Authorization": "Bearer ${API_TOKEN}",
				},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	inv, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: claude})
	if err != nil {
		t.Fatal(err)
	}
	got := mcpByName(inv.Servers)["private"]
	if got.Headers["Authorization"] != "Bearer ${API_TOKEN}" {
		t.Fatalf("load dropped headers: %+v", got)
	}

	acp := mcpServersToACP([]MCPServer{got})
	m := acp[0].(map[string]any)
	hdrs, ok := m["headers"].([]any)
	if !ok || len(hdrs) != 1 {
		t.Fatalf("acp headers = %#v", m["headers"])
	}
	bare := mcpServersToACP([]MCPServer{{Name: "mnemo", URL: "http://127.0.0.1:7700/mcp"}})
	if _, ok := bare[0].(map[string]any)["headers"].([]any); !ok {
		t.Fatalf("ACP HTTP must include headers array, got %#v", bare[0])
	}
}

func TestLoadCursorMCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	doc := map[string]any{
		"mcpServers": map[string]any{
			"jevonsmcp": map[string]any{"type": "http", "url": "http://127.0.0.1:13705/mcp"},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	inv, err := LoadMCP(&LoadMCPArgs{CursorJSON: path})
	if err != nil {
		t.Fatal(err)
	}
	got := mcpByName(inv.Servers)["jevonsmcp"]
	if got.URL != "http://127.0.0.1:13705/mcp" || !hasProvider(got.Providers, ProviderCursor) {
		t.Fatalf("cursor jevonsmcp = %+v", got)
	}
}

func TestLoadMCPMissingFileIsEmpty(t *testing.T) {
	inv, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: filepath.Join(t.TempDir(), "nope.json")})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Servers) != 0 {
		t.Fatalf("servers = %+v", inv.Servers)
	}
}

func mcpByName(servers []MCPServer) map[string]MCPServer {
	out := map[string]MCPServer{}
	for _, s := range servers {
		out[s.Name] = s
	}
	return out
}

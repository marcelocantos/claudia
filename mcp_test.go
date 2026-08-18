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

func TestEnsureMCPWritesClaudeGrokCodexAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude.json")
	grok := filepath.Join(dir, "grok.toml")
	codex := filepath.Join(dir, "codex.toml")
	if err := os.WriteFile(claude, []byte(`{"other":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grok, []byte("[mcp_servers.keep]\nurl = \"http://127.0.0.1:9/old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte("# preamble\nmodel = \"gpt-5\"\n\n[mcp_servers.keep]\ncommand = \"npx\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	args := &EnsureMCPArgs{
		Name:       "jevonsmcp",
		URL:        "http://127.0.0.1:13705/mcp",
		ClaudeJSON: claude,
		GrokTOML:   grok,
		CodexTOML:  codex,
	}
	if err := EnsureMCP(args); err != nil {
		t.Fatalf("EnsureMCP: %v", err)
	}
	if err := EnsureMCP(args); err != nil {
		t.Fatalf("second EnsureMCP: %v", err)
	}

	inv, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: claude})
	if err != nil {
		t.Fatal(err)
	}
	got := mcpByName(inv.Servers)["jevonsmcp"]
	if got.URL != args.URL || got.Type != "http" {
		t.Fatalf("claude jevonsmcp = %+v", got)
	}
	var claudeRoot map[string]any
	if err := json.Unmarshal(readFile(t, claude), &claudeRoot); err != nil {
		t.Fatal(err)
	}
	if claudeRoot["other"] != true {
		t.Fatal("claude ensure dropped unrelated keys")
	}

	grokTxt := string(readFile(t, grok))
	if !strings.Contains(grokTxt, `[mcp_servers.keep]`) || !strings.Contains(grokTxt, `http://127.0.0.1:9/old`) {
		t.Fatalf("grok lost keep:\n%s", grokTxt)
	}
	if strings.Count(grokTxt, `[mcp_servers.jevonsmcp]`) != 1 {
		t.Fatalf("grok jevonsmcp tables = %d\n%s", strings.Count(grokTxt, `[mcp_servers.jevonsmcp]`), grokTxt)
	}
	if !strings.Contains(grokTxt, args.URL) {
		t.Fatalf("grok missing url:\n%s", grokTxt)
	}

	codexTxt := string(readFile(t, codex))
	if !strings.Contains(codexTxt, "model = \"gpt-5\"") || !strings.Contains(codexTxt, `[mcp_servers.keep]`) {
		t.Fatalf("codex lost preamble:\n%s", codexTxt)
	}
	if strings.Count(codexTxt, `[mcp_servers.jevonsmcp]`) != 1 || !strings.Contains(codexTxt, args.URL) {
		t.Fatalf("codex jevonsmcp:\n%s", codexTxt)
	}

	beforeGrok := grokTxt
	if err := EnsureMCP(args); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, grok)) != beforeGrok {
		t.Fatal("third ensure rewrote an already-correct grok file")
	}
}

func TestEnsureMCPCorrectsStaleURL(t *testing.T) {
	dir := t.TempDir()
	grok := filepath.Join(dir, "grok.toml")
	if err := os.WriteFile(grok, []byte("[mcp_servers.jevonsmcp]\nurl = \"http://127.0.0.1:1/mcp\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:13705/mcp"
	if err := EnsureMCP(&EnsureMCPArgs{
		Name:      "jevonsmcp",
		URL:       want,
		GrokTOML:  grok,
		Providers: []Provider{ProviderGrok},
	}); err != nil {
		t.Fatal(err)
	}
	txt := string(readFile(t, grok))
	if !strings.Contains(txt, want) || strings.Contains(txt, "127.0.0.1:1/mcp") {
		t.Fatalf("stale url not replaced:\n%s", txt)
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
	if !argvHolds(args, "--mcp-config") || !argvHolds(args, claudiaMCPFile(req.WorkDir)) {
		t.Fatalf("claude argv missing session mcp file: %v", args)
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

func TestWriteSessionMCPFileIsPrivateJSON(t *testing.T) {
	dir := t.TempDir()
	req := agentStartRequest{
		WorkDir: dir,
		Config: Config{
			MCPServers: []MCPServer{{Name: "mnemo", URL: "http://127.0.0.1:7700/mcp"}},
		},
	}
	if err := writeSessionMCPFile(req); err != nil {
		t.Fatal(err)
	}
	inv, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: claudiaMCPFile(dir)})
	if err != nil {
		t.Fatal(err)
	}
	if mcpByName(inv.Servers)["mnemo"].URL != "http://127.0.0.1:7700/mcp" {
		t.Fatalf("session file = %+v", inv.Servers)
	}
}

func TestMCPAuthRoundTripAndACP(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude.json")
	grok := filepath.Join(dir, "grok.toml")
	codex := filepath.Join(dir, "codex.toml")
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

	args := &EnsureMCPArgs{
		Name:           "private",
		URL:            got.URL,
		Headers:        got.Headers,
		BearerTokenEnv: "API_TOKEN",
		ClaudeJSON:     claude,
		GrokTOML:       grok,
		CodexTOML:      codex,
	}
	if err := EnsureMCP(args); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMCP(args); err != nil {
		t.Fatal(err)
	}
	again, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: claude})
	if err != nil {
		t.Fatal(err)
	}
	got = mcpByName(again.Servers)["private"]
	if got.Headers["Authorization"] != "Bearer ${API_TOKEN}" || got.BearerTokenEnv != "API_TOKEN" {
		t.Fatalf("ensure dropped auth: %+v", got)
	}
	grokTxt := string(readFile(t, grok))
	if !strings.Contains(grokTxt, "Bearer ${API_TOKEN}") || !strings.Contains(grokTxt, "bearer_token_env_var") {
		t.Fatalf("grok auth:\n%s", grokTxt)
	}
	codexTxt := string(readFile(t, codex))
	if !strings.Contains(codexTxt, `bearer_token_env_var = "API_TOKEN"`) {
		t.Fatalf("codex auth:\n%s", codexTxt)
	}
	acp := mcpServersToACP([]MCPServer{got})
	m := acp[0].(map[string]any)
	hdrs, ok := m["headers"].([]any)
	if !ok || len(hdrs) != 1 {
		t.Fatalf("acp headers = %#v", m["headers"])
	}
}

func TestEnsureMCPCorrectsStaleHeader(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude.json")
	if err := EnsureMCP(&EnsureMCPArgs{
		Name:       "api",
		URL:        "https://mcp.example.com/mcp",
		Headers:    map[string]string{"Authorization": "Bearer old"},
		ClaudeJSON: claude,
		Providers:  []Provider{ProviderClaude},
	}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMCP(&EnsureMCPArgs{
		Name:       "api",
		URL:        "https://mcp.example.com/mcp",
		Headers:    map[string]string{"Authorization": "Bearer new"},
		ClaudeJSON: claude,
		Providers:  []Provider{ProviderClaude},
	}); err != nil {
		t.Fatal(err)
	}
	inv, err := LoadMCP(&LoadMCPArgs{ClaudeJSON: claude})
	if err != nil {
		t.Fatal(err)
	}
	if mcpByName(inv.Servers)["api"].Headers["Authorization"] != "Bearer new" {
		t.Fatalf("stale header not replaced: %+v", inv.Servers)
	}
}

func TestEnsureMCPRejectsBadName(t *testing.T) {
	err := EnsureMCP(&EnsureMCPArgs{Name: "bad name", URL: "http://127.0.0.1/mcp"})
	if err == nil {
		t.Fatal("expected invalid name")
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

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

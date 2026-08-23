// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPExclusiveClaudeArgvUsesStrictConfig(t *testing.T) {
	req := agentStartRequest{WorkDir: "/work/t45", Config: Config{MCPExclusive: true}}
	args := claudeAgentArgs(req)
	if !argvHolds(args, "--strict-mcp-config") || !argvHolds(args, "--mcp-config") {
		t.Fatalf("exclusive argv missing isolate flags: %v", args)
	}
	add := claudeAgentArgs(agentStartRequest{WorkDir: "/work/t45", Config: Config{}})
	if argvHolds(add, "--strict-mcp-config") {
		t.Fatalf("additive argv must not isolate: %v", add)
	}
}

func TestMCPExclusiveGrokPlanSetsHome(t *testing.T) {
	req := agentStartRequest{WorkDir: "/work/t45", Config: Config{MCPExclusive: true}}
	plan := planGrokSession(req)
	if plan.GrokHome == "" || !strings.Contains(plan.GrokHome, "grok") {
		t.Fatalf("GrokHome = %q", plan.GrokHome)
	}
	add := planGrokSession(agentStartRequest{WorkDir: "/work/t45", Config: Config{}})
	if add.GrokHome != "" {
		t.Fatalf("additive GrokHome = %q", add.GrokHome)
	}
}

func TestPrepareExclusiveHomesSkipUserMCP(t *testing.T) {
	dir := t.TempDir()
	grok, err := prepareExclusiveGrokHome(dir)
	if err != nil {
		t.Fatal(err)
	}
	txt, err := os.ReadFile(filepath.Join(grok, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(txt), "[mcp_servers") {
		t.Fatalf("exclusive grok home leaked mcp_servers:\n%s", txt)
	}
	if !strings.Contains(string(txt), "mcps = false") {
		t.Fatalf("exclusive grok home must disable Claude MCP compat:\n%s", txt)
	}
	codex, err := prepareExclusiveCodexHome(dir, []MCPServer{
		{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"},
		{Name: "stdio", Command: "/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgb, err := os.ReadFile(filepath.Join(codex, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(cfgb)
	if !strings.Contains(cfg, "[mcp_servers.onlyme]") || strings.Contains(cfg, "stdio") {
		t.Fatalf("exclusive codex home:\n%s", cfg)
	}
}

func TestMCPExclusiveCursorPlanSetsProjectMCP(t *testing.T) {
	req := agentStartRequest{WorkDir: "/work/t45", Config: Config{MCPExclusive: true}}
	plan := planCursorSession(req)
	want := filepath.Join("/work/t45", ".cursor", "mcp.json")
	if plan.ExclusiveProjectMCP != want {
		t.Fatalf("ExclusiveProjectMCP = %q, want %q", plan.ExclusiveProjectMCP, want)
	}
	add := planCursorSession(agentStartRequest{WorkDir: "/work/t45", Config: Config{}})
	if add.ExclusiveProjectMCP != "" {
		t.Fatalf("additive ExclusiveProjectMCP = %q", add.ExclusiveProjectMCP)
	}
}

func TestWriteExclusiveCursorProjectMCPWritesOnlyNamedServers(t *testing.T) {
	dir := t.TempDir()
	path, err := writeExclusiveCursorProjectMCP(dir, []MCPServer{
		{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"},
		{Name: "stdio", Command: "/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := LoadMCP(&LoadMCPArgs{CursorJSON: path})
	if err != nil {
		t.Fatal(err)
	}
	byName := mcpByName(inv.Servers)
	if byName["onlyme"].URL != "http://127.0.0.1:9/mcp" {
		t.Fatalf("exclusive cursor mcp = %+v", inv.Servers)
	}
	if _, ok := byName["stdio"]; ok {
		t.Fatalf("stdio leaked into exclusive cursor mcp: %+v", inv.Servers)
	}
}

func TestEnsureDefMCPSkipsWhenExclusive(t *testing.T) {
	if err := ensureDefMCP(&AgentDef{Provider: ProviderCodex, MCPExclusive: true, MCPServers: []MCPServer{{Name: "x", URL: "http://127.0.0.1:9/mcp"}}}); err != nil {
		t.Fatal(err)
	}
}

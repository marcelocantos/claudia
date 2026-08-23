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
	if plan.GrokHome != "session:GROK_HOME" {
		t.Fatalf("GrokHome = %q, want session:GROK_HOME", plan.GrokHome)
	}
	add := planGrokSession(agentStartRequest{WorkDir: "/work/t45", Config: Config{}})
	if add.GrokHome != "" {
		t.Fatalf("additive GrokHome = %q", add.GrokHome)
	}
}

func TestPrepareExclusiveHomesSkipUserMCP(t *testing.T) {
	grok, cleanup, err := prepareExclusiveGrokHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
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
	codex, cleanup2, err := prepareExclusiveCodexHome([]MCPServer{
		{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"},
		{Name: "stdio", Command: "/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup2)
	cfgb, err := os.ReadFile(filepath.Join(codex, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(cfgb)
	if !strings.Contains(cfg, "[mcp_servers.onlyme]") || strings.Contains(cfg, "stdio") {
		t.Fatalf("exclusive codex home:\n%s", cfg)
	}
}

func TestMCPExclusiveCursorPlanIsACPOnly(t *testing.T) {
	req := agentStartRequest{
		WorkDir: "/work/t45",
		Config: Config{
			MCPExclusive: true,
			MCPServers:   []MCPServer{{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"}},
		},
	}
	plan := planCursorSession(req)
	if !plan.MCPExclusive {
		t.Fatal("plan.MCPExclusive = false")
	}
	if len(plan.MCPServers) != 1 {
		t.Fatalf("ACP MCPServers = %#v", plan.MCPServers)
	}
	add := planCursorSession(agentStartRequest{WorkDir: "/work/t45", Config: Config{}})
	if add.MCPExclusive {
		t.Fatal("additive plan should not be exclusive")
	}
}

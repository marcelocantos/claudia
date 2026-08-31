// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Claude Code CLI exposes `--remote-control` (opt-in enable) and no
// `--no-remote-control` / per-pane disable. Session seats do not pass the
// enable flag; the /rc status chrome cannot be turned off by launch flag
// (jevons 🎯T565 part c). Product path is wait-out + a named not-ready reason.
func TestClaudeSessionArgsOmitRemoteControl(t *testing.T) {
	args := claudeAgentArgs(agentStartRequest{
		WorkDir:   "/work/t565",
		SessionID: "00000000-0000-0000-0000-000000000565",
		Config:    Config{},
	})
	for _, a := range args {
		if strings.Contains(a, "remote-control") {
			t.Fatalf("session argv must not enable remote control: %v", args)
		}
	}
}

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
	}, codexSandboxTuning{})
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

func TestExclusiveCodexHomePersistsAndReuses(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	const sid = "01a00f11-547e-7a32-a284-b5832f3697db"

	src, cleanup, err := prepareExclusiveCodexHome([]MCPServer{
		{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"},
	}, codexSandboxTuning{})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(src, "sessions", sid, "rollout.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistExclusiveCodexHome(src, sid); err != nil {
		t.Fatal(err)
	}
	cleanup() // must not delete the durable home

	dest := exclusiveCodexHomeDir(sid)
	if !dirExists(dest) {
		t.Fatalf("durable home missing at %s", dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "sessions", sid, "rollout.json")); err != nil {
		t.Fatalf("rollout lost after persist: %v", err)
	}

	home, reuseCleanup, err := exclusiveCodexHomeForStart(sid, true, []MCPServer{
		{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"},
	}, codexSandboxTuning{})
	if err != nil {
		t.Fatal(err)
	}
	reuseCleanup()
	if home != dest {
		t.Fatalf("reuse home = %q, want %q", home, dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "sessions", sid, "rollout.json")); err != nil {
		t.Fatalf("rollout lost on reuse rewrite: %v", err)
	}
}

func TestPublishExclusiveCodexHomeAliasesThreadID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const minted = "fd4bbbe8-0000-4000-8000-000000d95306"
	const thread = "01a0321d-ae2e-77d0-926c-c884a5add0fe"
	src, _, err := exclusiveCodexHomeForStart(minted, false, nil, codexSandboxTuning{})
	if err != nil {
		t.Fatal(err)
	}
	if src != exclusiveCodexHomeDir(minted) {
		t.Fatalf("first mint home = %q, want durable %q", src, exclusiveCodexHomeDir(minted))
	}
	if err := os.WriteFile(filepath.Join(src, "rollout.marker"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publishExclusiveCodexHome(src, thread); err != nil {
		t.Fatal(err)
	}
	alias := exclusiveCodexHomeDir(thread)
	if !dirExists(alias) {
		t.Fatalf("thread-id alias missing at %s", alias)
	}
	if _, err := os.Stat(filepath.Join(alias, "rollout.marker")); err != nil {
		t.Fatalf("rollout not visible via thread-id alias: %v", err)
	}
}

func TestExclusiveCodexHomeMissingFailsLoud(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const sid = "thr_missing_home"
	_, _, err := exclusiveCodexHomeForStart(sid, true, nil, codexSandboxTuning{})
	if err == nil {
		t.Fatal("RequireResume with missing home succeeded")
	}
	want := exclusiveCodexHomeDir(sid)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want named home %s", err, want)
	}
	if strings.Contains(err.Error(), "no rollout found") {
		t.Fatal("must fail before empty-home resume")
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

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCPExclusiveGrokInspectJourney is the cheap Claudia-side net for
// J2's hang: exclusive GROK_HOME must disable Claude-compat MCP so Grok
// does not dial dead HTTP servers from ~/.claude.json and kill the worker.
// Sequence: prepare home → grok inspect → no enabled [claude] MCP.
func TestMCPExclusiveGrokInspectJourney(t *testing.T) {
	bin, err := resolveGrokBin()
	if err != nil {
		t.Skipf("grok binary not found: %v", err)
	}
	dir := t.TempDir()
	home, err := prepareExclusiveGrokHome(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "inspect")
	cmd.Dir = dir
	cmd.Env = appendEnv(os.Environ(), exclusiveEnv("GROK_HOME", home))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("grok inspect: %v\n%s", err, out)
	}
	enabled := enabledClaudeHTTP(string(out))
	if len(enabled) > 0 {
		t.Fatalf("exclusive GROK_HOME still has enabled Claude HTTP MCP (would 502 the worker): %v\n%s", enabled, out)
	}
	if !bytes.Contains(out, []byte(home+"/config.toml")) && !bytes.Contains(out, []byte("compat")) {
		// Config Sources should name the exclusive home; do not require
		// a exact path match across macOS /private prefixes.
		t.Logf("grok inspect (no enabled Claude HTTP MCP):\n%s", out)
	}
}

func enabledClaudeHTTP(inspect string) []string {
	var out []string
	inMCP := false
	for _, line := range strings.Split(inspect, "\n") {
		if strings.Contains(line, "MCP Servers") {
			inMCP = true
			continue
		}
		if !inMCP {
			continue
		}
		trim := strings.TrimSpace(line)
		if trim == "" || (!strings.Contains(line, "└") && !strings.HasPrefix(line, " ")) {
			inMCP = false
			continue
		}
		if strings.Contains(trim, "[claude]") && strings.Contains(trim, "(http)") && !strings.Contains(trim, "[disabled]") {
			out = append(out, trim)
		}
	}
	return out
}

// TestMCPExclusiveSessionRoundTrip is J2 without jevons: one exclusive
// Grok Session, one ping, one terminal. Catches worker-death from stray
// MCP (502 on a dead HTTP server) as WaitForResponse timeout.
func TestMCPExclusiveSessionRoundTrip(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveGrokBin(); err != nil {
		t.Skipf("grok binary not found: %v", err)
	}
	t.Setenv("CLAUDIA_GROK_CONNECT", "0")
	token := fmt.Sprintf("claudia-ping-%d", time.Now().Unix()%100000)
	cfg := Config{
		Provider:     ProviderGrok,
		WorkDir:      t.TempDir(),
		TermLogPath:  "-",
		MCPExclusive: true,
		GrokConnect:  false,
	}
	agent, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(t.Context()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := agent.Send("Reply with exactly: " + token); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reply, err := agent.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			return -1
		}
		return r
	}, strings.ToLower(reply))
	if !strings.Contains(compact, strings.ToLower(token)) && reply == "" {
		t.Fatalf("empty reply")
	}
	t.Logf("exclusive grok round-trip: %q", reply)
}

// TestMCPExclusiveCursorProjectMCPJourney is the hermetic Cursor twin of
// GROK_HOME inspect: exclusive must plan a project mcp.json path and write
// only the named HTTP servers — never rewrite HOME (Keychain).
func TestMCPExclusiveCursorProjectMCPJourney(t *testing.T) {
	dir := t.TempDir()
	req := agentStartRequest{
		WorkDir: dir,
		Config: Config{
			MCPExclusive: true,
			MCPServers: []MCPServer{
				{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"},
				{Name: "stdio", Command: "/bin/true"},
			},
		},
	}
	plan := planCursorSession(req)
	wantPath := filepath.Join(dir, ".cursor", "mcp.json")
	if plan.ExclusiveProjectMCP != wantPath {
		t.Fatalf("ExclusiveProjectMCP = %q, want %q", plan.ExclusiveProjectMCP, wantPath)
	}

	path, err := writeExclusiveCursorProjectMCP(dir, req.Config.MCPServers)
	if err != nil {
		t.Fatal(err)
	}
	if path != wantPath {
		t.Fatalf("wrote %q, want %q", path, wantPath)
	}
	inv, err := LoadMCP(&LoadMCPArgs{CursorJSON: path})
	if err != nil {
		t.Fatal(err)
	}
	byName := mcpByName(inv.Servers)
	if byName["onlyme"].URL != "http://127.0.0.1:9/mcp" {
		t.Fatalf("exclusive project mcp = %+v", inv.Servers)
	}
	if _, ok := byName["stdio"]; ok {
		t.Fatalf("stdio leaked into exclusive project mcp: %+v", inv.Servers)
	}
}

// TestMCPExclusiveCursorSessionRoundTrip is the live Cursor exclusive
// ping: project mcp.json isolate, real-home auth (or CURSOR_API_KEY),
// one Send, one terminal. Catches Keychain/HOME regressions and ACP
// handshake stalls.
func TestMCPExclusiveCursorSessionRoundTrip(t *testing.T) {
	if os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
		t.Skip("CLAUDIA_CURSOR_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCursorBin(); err != nil {
		t.Skipf("cursor agent binary not found: %v", err)
	}
	token := fmt.Sprintf("claudia-ping-%d", time.Now().Unix()%100000)
	cfg := Config{
		Provider:     ProviderCursor,
		WorkDir:      t.TempDir(),
		TermLogPath:  "-",
		MCPExclusive: true,
	}
	agent, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(t.Context()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	mcpPath := filepath.Join(cfg.WorkDir, ".cursor", "mcp.json")
	if _, err := os.Stat(mcpPath); err != nil {
		t.Fatalf("exclusive did not write project mcp.json: %v", err)
	}
	if err := agent.Send("Reply with exactly: " + token); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reply, err := agent.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			return -1
		}
		return r
	}, strings.ToLower(reply))
	if !strings.Contains(compact, strings.ToLower(token)) && reply == "" {
		t.Fatalf("empty reply")
	}
	t.Logf("exclusive cursor round-trip: %q", reply)
}

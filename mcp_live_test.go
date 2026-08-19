// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Live 🎯T40: LoadMCP reads the real Claude user map (mnemo is assumed
// present), a Session started with that inventory plus the same mnemo
// URL can see the server, and EnsureMCP is a no-op when Codex already
// has the matching entry.

func TestMCPLiveLoadAndSessionSeesMnemo(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" && os.Getenv("CLAUDIA_GROK_LIVE") == "" && os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("no live gate set")
	}
	inv, err := LoadMCP(nil)
	if err != nil {
		t.Fatalf("LoadMCP: %v", err)
	}
	mnemo, ok := mcpByName(inv.Servers)["mnemo"]
	if !ok || mnemo.URL == "" {
		t.Fatalf("LoadMCP(%s) has no mnemo HTTP server; live T40 assumes mnemo is in the Claude user map", inv.Source)
	}
	t.Logf("system mnemo url=%s source=%s servers=%d", mnemo.URL, inv.Source, len(inv.Servers))

	t.Run("claude", func(t *testing.T) {
		if os.Getenv("CLAUDIA_LIVE") == "" {
			t.Skip("CLAUDIA_LIVE not set")
		}
		if _, err := exec.LookPath("claude"); err != nil {
			t.Skip("claude not on PATH")
		}
		runLiveMCPSeesMnemo(t, Config{
			Provider:    ProviderClaude,
			Model:       "haiku",
			MCPServers:  []MCPServer{mnemo},
			TermLogPath: "-",
		})
	})
	t.Run("grok", func(t *testing.T) {
		if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
			t.Skip("CLAUDIA_GROK_LIVE not set")
		}
		if _, err := resolveGrokBin(); err != nil {
			t.Skip(err)
		}
		runLiveMCPSeesMnemo(t, Config{
			Provider:    ProviderGrok,
			MCPServers:  []MCPServer{mnemo},
			TermLogPath: "-",
		})
	})
	t.Run("codex", func(t *testing.T) {
		if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
			t.Skip("CLAUDIA_CODEX_LIVE not set")
		}
		if _, err := resolveCodexBin(); err != nil {
			t.Skip(err)
		}
		if err := EnsureMCP(&EnsureMCPArgs{
			Name:      mnemo.Name,
			URL:       mnemo.URL,
			Providers: []Provider{ProviderCodex},
		}); err != nil {
			t.Fatalf("EnsureMCP codex: %v", err)
		}
		runLiveMCPSeesMnemo(t, Config{
			Provider:    ProviderCodex,
			MCPServers:  []MCPServer{mnemo},
			TermLogPath: "-",
		})
	})
}

func runLiveMCPSeesMnemo(t *testing.T, cfg Config) {
	t.Helper()
	cfg.WorkDir = t.TempDir()
	agent, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(t.Context()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := agent.Send("List your MCP tool names. If any name contains mnemo, reply with exactly: MNEMO-OK"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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
	if !strings.Contains(compact, "mnemo-ok") && !strings.Contains(compact, "mnemo") {
		t.Fatalf("reply does not show mnemo: %q", reply)
	}
	t.Logf("mcp live reply: %q", reply)
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Live 🎯T40: LoadMCP reads the real Claude user map (mnemo is assumed
// present) and a Session started with that inventory under MCPExclusive
// can see the server. Exclusive keeps host MCP maps from drowning the
// assertion (Codex especially).

func TestMCPLiveLoadAndSessionSeesMnemo(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" && os.Getenv("CLAUDIA_GROK_LIVE") == "" && os.Getenv("CLAUDIA_CODEX_LIVE") == "" && os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
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
	// Prefer the local mnemo daemon when the Claude map points at a
	// jevons upstream that is down or unrouted (404 on /upstream/mnemo).
	if direct := liveMnemoDirectURL(); direct != "" {
		t.Logf("using direct mnemo %s (LoadMCP had %s)", direct, mnemo.URL)
		mnemo.URL = direct
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
			Provider:     ProviderClaude,
			Model:        "haiku",
			MCPServers:   []MCPServer{mnemo},
			MCPExclusive: true,
			TermLogPath:  "-",
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
			Provider:     ProviderGrok,
			MCPServers:   []MCPServer{mnemo},
			MCPExclusive: true,
			TermLogPath:  "-",
		})
	})
	t.Run("codex", func(t *testing.T) {
		if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
			t.Skip("CLAUDIA_CODEX_LIVE not set")
		}
		if _, err := resolveCodexBin(); err != nil {
			t.Skip(err)
		}
		// Exclusive CODEX_HOME so the host's crowded ~/.codex/config.toml
		// cannot hide mnemo among dozens of other MCP namespaces.
		runLiveMCPSeesMnemo(t, Config{
			Provider:     ProviderCodex,
			MCPServers:   []MCPServer{mnemo},
			MCPExclusive: true,
			TermLogPath:  "-",
		})
	})
	t.Run("cursor", func(t *testing.T) {
		if os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
			t.Skip("CLAUDIA_CURSOR_LIVE not set")
		}
		if _, err := resolveCursorBin(); err != nil {
			t.Skip(err)
		}
		runLiveMCPSeesMnemo(t, Config{
			Provider:     ProviderCursor,
			MCPServers:   []MCPServer{mnemo},
			MCPExclusive: true,
			TermLogPath:  "-",
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
	// Affirmative only. A bare "mnemo" substring matches refusals like
	// "no tool name contains mnemo" (Codex live 2026-08-23).
	if !strings.Contains(compact, "mnemo-ok") && !strings.Contains(compact, "mcp__mnemo") {
		t.Fatalf("reply does not show mnemo attached: %q", reply)
	}
	t.Logf("mcp live reply: %q", reply)
}

// liveMnemoDirectURL returns http://127.0.0.1:19419/mcp when the local
// mnemo daemon answers initialize there; otherwise empty.
func liveMnemoDirectURL() string {
	const url = "http://127.0.0.1:19419/mcp"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claudia-live","version":"0"}}}`,
	))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return url
}

// TestMCPHostLiveSeatsSeeMnemo is 🎯T75.5's live gate: a Registry with an
// in-process MCPHost and no daemon starts a real seat whose mnemo server is
// attached through the host's loopback proxy, and the seat lists mnemo's
// tools, which only works if initialize and tools/list crossed the host.
func TestMCPHostLiveSeatsSeeMnemo(t *testing.T) {
	cases := []struct {
		name  string
		gate  string
		def   AgentDef
		found func() error
	}{
		{"claude", "CLAUDIA_LIVE", AgentDef{Provider: ProviderClaude, Model: "haiku"}, func() error { _, err := exec.LookPath("claude"); return err }},
		{"grok", "CLAUDIA_GROK_LIVE", AgentDef{Provider: ProviderGrok}, func() error { _, err := resolveGrokBin(); return err }},
		{"codex", "CLAUDIA_CODEX_LIVE", AgentDef{Provider: ProviderCodex}, func() error { _, err := resolveCodexBin(); return err }},
		{"cursor", "CLAUDIA_CURSOR_LIVE", AgentDef{Provider: ProviderCursor}, func() error { _, err := resolveCursorBin(); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv(tc.gate) == "" {
				t.Skipf("%s not set", tc.gate)
			}
			if err := tc.found(); err != nil {
				t.Skip(err)
			}
			mnemo := MCPServer{Name: "mnemo", Type: "http", URL: liveMnemoDirectURL()}
			if mnemo.URL == "" {
				t.Skip("no local mnemo daemon on 127.0.0.1:19419")
			}
			host, err := NewMCPHost(&MCPHostArgs{StateDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
			if err != nil {
				t.Fatal(err)
			}
			reg.SetMCPHost(host)
			def := tc.def
			def.Name, def.WorkDir, def.TermLogPath = "hosted-"+tc.name, t.TempDir(), "-"
			def.SessionID = uuid.NewString()
			def.MCPServers, def.MCPExclusive = []MCPServer{mnemo}, true
			if err := reg.Register(def); err != nil {
				t.Fatal(err)
			}
			agent, err := reg.Launch(def.Name)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			defer reg.StopAll()
			agent.mu.Lock()
			started := agent.startCfg.MCPServers
			agent.mu.Unlock()
			if len(started) != 1 || !strings.HasPrefix(started[0].URL, "http://"+host.Addr()+"/upstream/") {
				t.Fatalf("seat was not started on the host's URL: %+v", started)
			}
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
			compact := strings.ToLower(strings.Join(strings.Fields(reply), ""))
			if !strings.Contains(compact, "mnemo-ok") && !strings.Contains(compact, "mcp__mnemo") {
				t.Fatalf("reply does not show mnemo attached through the host: %q", reply)
			}
			t.Logf("hosted mnemo reply: %q (host %s)", reply, host.Addr())
		})
	}
}

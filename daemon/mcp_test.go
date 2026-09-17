// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
)

// TestGrantAttachesHostedMCP (🎯T2.16): a seat the daemon starts reaches its
// stdio MCP server through the daemon's host, and a consumer-owned jevonsmcp
// server keeps the caller's URL.
func TestGrantAttachesHostedMCP(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	f := newFixture(t)
	f.boot(t, nil)
	if f.d.mcp == nil {
		t.Fatal("daemon has no mcp host")
	}
	a, err := claudia.Start(claudia.Config{
		Name:        "mcp-seat",
		WorkDir:     t.TempDir(),
		SessionID:   "sid-mcp",
		TermLogPath: "-",
		MCPServers: []claudia.MCPServer{
			{Name: "fixture", Command: bin},
			{Name: "jevonsmcp", Type: "http", URL: "http://127.0.0.1:13705/mcp"},
		},
	})
	if err != nil {
		t.Fatalf("Start via daemon: %v", err)
	}
	t.Cleanup(a.Stop)
	var fixture, jevons claudia.MCPServer
	for _, s := range f.seat(0).start().Config.MCPServers {
		switch s.Name {
		case "fixture":
			fixture = s
		case "jevonsmcp":
			jevons = s
		}
	}
	if fixture.Command != "" || !strings.HasSuffix(fixture.URL, "/upstream/fixture") {
		t.Fatalf("daemon did not host fixture: %+v", f.seat(0).start().Config.MCPServers)
	}
	if jevons.URL != "http://127.0.0.1:13705/mcp" {
		t.Fatalf("jevonsmcp rewritten: %+v", jevons)
	}
}

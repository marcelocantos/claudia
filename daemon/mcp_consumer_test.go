// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
)

func TestMCPConsumerConfigurationIsExplicit(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "neutral"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			seed := filepath.Join(home, ".jevons")
			if err := os.Mkdir(seed, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(seed, "mcp_oauth_tokens.json"), []byte(`{"fixture":{"AccessToken":"fixture-token"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := ""
				if explicit {
					want = "Bearer fixture-token"
				}
				if got := r.Header.Get("Authorization"); got != want {
					t.Errorf("MCP token=%q, want %q", got, want)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"fixture-ok"}]}}`))
			}))
			defer upstream.Close()
			f := newFixture(t)
			opts := f.options(nil)
			opts.DisableResume = true
			if explicit {
				opts.MCPSeedStateDirs = []string{seed}
				opts.MCPConsumerOwnedPrefixes = []string{"jevonsmcp"}
			}
			f.bootWith(t, opts)
			servers := f.d.mcp.Attach([]claudia.MCPServer{
				{Name: "fixture", Type: "http", URL: upstream.URL},
				{Name: "jevonsmcp-local", Type: "http", URL: upstream.URL},
			})
			if (servers[1].URL == upstream.URL) != explicit {
				t.Fatalf("consumer route=%q, explicit=%v", servers[1].URL, explicit)
			}
			if got := claudia.CallMCPTool(servers[0].URL, "fixture", `{}`); !strings.Contains(got, "fixture-ok") {
				t.Fatalf("seeded MCP host did not serve tool: %s", got)
			}
		})
	}
}

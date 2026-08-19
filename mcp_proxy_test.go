// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPProxyOpenPassThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("open upstream got Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	}))
	t.Cleanup(up.Close)

	p, err := NewMCPProxy(&MCPProxyArgs{
		Prefix:     "/upstream",
		PublicBase: "http://127.0.0.1:13705",
		Servers:    []MCPServer{{Name: "mnemo", URL: up.URL + "/mcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.PublicURL("mnemo"); got != "http://127.0.0.1:13705/upstream/mnemo" {
		t.Fatalf("PublicURL = %q", got)
	}
	adv := p.Advertised()
	if len(adv) != 1 || adv[0].URL != "http://127.0.0.1:13705/upstream/mnemo" || adv[0].Headers != nil {
		t.Fatalf("Advertised = %+v", adv)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upstream/mnemo", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestMCPProxyStaticRetriesWithHeader(t *testing.T) {
	var sawAuth atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer secret" {
			sawAuth.Store(true)
			io.WriteString(w, `{"ok":true}`)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(up.Close)

	p, err := NewMCPProxy(&MCPProxyArgs{
		Prefix: "/upstream",
		Servers: []MCPServer{{
			Name:    "private",
			URL:     up.URL,
			Headers: map[string]string{"Authorization": "Bearer secret"},
		}},
		Probe: func(ctx context.Context, rawURL string) (*MCPProbe, error) {
			return &MCPProbe{Kind: MCPAuthStatic, URL: rawURL, Status: 401}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upstream/private", strings.NewReader(`{}`))
	p.ServeHTTP(rec, req)
	if rec.Code != 200 || !sawAuth.Load() {
		t.Fatalf("status=%d auth=%v body=%s", rec.Code, sawAuth.Load(), rec.Body.String())
	}
}

func TestMCPProxyOAuthRetriesWithToken(t *testing.T) {
	var authorized atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-xyz" {
			io.WriteString(w, `{"ok":true}`)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://example/.well-known/oauth-protected-resource"`)
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(up.Close)

	p, err := NewMCPProxy(&MCPProxyArgs{
		Prefix:  "/upstream",
		Servers: []MCPServer{{Name: "atlassian", URL: up.URL}},
		Probe: func(ctx context.Context, rawURL string) (*MCPProbe, error) {
			return &MCPProbe{Kind: MCPAuthOAuth, URL: rawURL, Status: 401, ResourceMetadata: "http://example/.well-known"}, nil
		},
		Authorize: func(ctx context.Context, args *AuthorizeMCPArgs) (*MCPToken, error) {
			authorized.Store(true)
			return &MCPToken{AccessToken: "access-xyz", TokenType: "Bearer"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upstream/atlassian", strings.NewReader(`{}`))
	p.ServeHTTP(rec, req)
	if rec.Code != 200 || !authorized.Load() {
		t.Fatalf("status=%d authorized=%v body=%s", rec.Code, authorized.Load(), rec.Body.String())
	}
}

func TestMCPProxyUnknownAndStdioAre404(t *testing.T) {
	p, err := NewMCPProxy(&MCPProxyArgs{
		Prefix:  "/upstream",
		Servers: []MCPServer{{Name: "bullseye", Command: "/bin/true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/upstream/bullseye", "/upstream/missing", "/upstream", "/"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
	}
}

func TestMCPProxyStripPrefixViaHost(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(up.Close)
	inner, err := NewMCPProxy(&MCPProxyArgs{
		Servers: []MCPServer{{Name: "mnemo", URL: up.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/upstream/", http.StripPrefix("/upstream", inner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upstream/mnemo", strings.NewReader(`{}`)))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProbeMCPClassifiesOpenStaticOAuth(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var mcpURL, prmURL string
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mcpURL = srv.URL + "/mcp"
	prmURL = srv.URL + "/.well-known/oauth-protected-resource/mcp"

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Mode") {
		case "open":
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case "static":
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+prmURL+`", error="invalid_token"`)
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
		}
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"resource":              mcpURL,
			"authorization_servers": []string{srv.URL},
			"scopes_supported":      []string{"read"},
		})
	})

	ctx := t.Context()
	oauth, err := ProbeMCP(ctx, mcpURL)
	if err != nil {
		t.Fatal(err)
	}
	if oauth.Kind != MCPAuthOAuth || oauth.ResourceMetadata != prmURL || len(oauth.AuthorizationServers) != 1 {
		t.Fatalf("oauth probe = %+v", oauth)
	}

	req := func(mode string) *MCPProbe {
		t.Helper()
		// ProbeMCP uses DefaultClient; stamp mode via a redirecting wrapper
		// would be messy. Hit probeMCP with a client that sets X-Mode.
		c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r.Header.Set("X-Mode", mode)
			return http.DefaultTransport.RoundTrip(r)
		})}
		p, err := probeMCP(ctx, c, mcpURL)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := req("open"); p.Kind != MCPAuthOpen {
		t.Fatalf("open = %s", p.Kind)
	}
	if p := req("static"); p.Kind != MCPAuthStatic {
		t.Fatalf("static = %s", p.Kind)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthorizeMCPCompletesPKCEAgainstFixture(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mcpURL := srv.URL + "/mcp"
	prmURL := srv.URL + "/.well-known/oauth-protected-resource/mcp"

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+prmURL+`"`)
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"resource":              mcpURL,
			"authorization_servers": []string{srv.URL},
			"scopes_supported":      []string{"read"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           srv.URL,
			"authorization_endpoint":           srv.URL + "/authorize",
			"token_endpoint":                   srv.URL + "/token",
			"registration_endpoint":            srv.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "cid-1"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "good-code" {
			http.Error(w, "bad grant", 400)
			return
		}
		if r.Form.Get("code_verifier") == "" || r.Form.Get("client_id") != "cid-1" {
			http.Error(w, "pkce", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-xyz",
			"refresh_token": "refresh-xyz",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tok, err := AuthorizeMCP(ctx, &AuthorizeMCPArgs{
		URL: mcpURL,
		OpenURL: func(auth string) error {
			u, err := url.Parse(auth)
			if err != nil {
				return err
			}
			cb := u.Query().Get("redirect_uri")
			st := u.Query().Get("state")
			if cb == "" || st == "" {
				return err
			}
			go func() {
				time.Sleep(20 * time.Millisecond)
				_, _ = http.Get(cb + "?code=good-code&state=" + url.QueryEscape(st))
			}()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("AuthorizeMCP: %v", err)
	}
	if tok.AccessToken != "access-xyz" || tok.RefreshToken != "refresh-xyz" {
		t.Fatalf("token = %+v", tok)
	}
}

func TestProbeMCPLiveAtlassian(t *testing.T) {
	if os.Getenv("CLAUDIA_MCP_OAUTH_LIVE") == "" {
		t.Skip("CLAUDIA_MCP_OAUTH_LIVE not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := ProbeMCP(ctx, "https://mcp.atlassian.com/v1/mcp/authv2")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != MCPAuthOAuth || p.ResourceMetadata == "" {
		t.Fatalf("atlassian probe = %+v", p)
	}
	t.Logf("atlassian: status=%d meta=%s as=%v", p.Status, p.ResourceMetadata, p.AuthorizationServers)
}

func TestParseResourceMetadata(t *testing.T) {
	t.Parallel()
	got := parseResourceMetadata(`Bearer resource_metadata="https://ex/.well-known/oauth-protected-resource/v1", error="invalid_token"`)
	if !strings.Contains(got, "oauth-protected-resource") {
		t.Fatalf("got %q", got)
	}
	if parseResourceMetadata(`Bearer error="invalid_token"`) != "" {
		t.Fatal("expected empty")
	}
}

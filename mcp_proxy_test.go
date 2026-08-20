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
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestMCPProxySetTokenAndOnTokenChange(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer reseeded" {
			io.WriteString(w, `{"ok":true}`)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://example/.well-known"`)
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(up.Close)
	p, err := NewMCPProxy(&MCPProxyArgs{
		Prefix:  "/upstream",
		Servers: []MCPServer{{Name: "atlassian", URL: up.URL}},
		Probe: func(ctx context.Context, rawURL string) (*MCPProbe, error) {
			return &MCPProbe{Kind: MCPAuthOAuth, URL: rawURL, Status: 401, ResourceMetadata: "http://example/.well-known"}, nil
		},
		Authorize: func(ctx context.Context, args *AuthorizeMCPArgs) (*MCPToken, error) {
			t.Fatal("should not authorize after SetToken")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetToken("atlassian", &MCPToken{AccessToken: "reseeded", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upstream/atlassian", strings.NewReader(`{}`)))
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
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

// jevons 🎯T520: expired access + valid refresh → second upstream attempt
// succeeds; Authorize (browser) is never called.
func TestMCPProxyRefreshOnExpiredAccessNoAuthorize(t *testing.T) {
	var upstreamHits atomic.Int32
	var authorizeHits atomic.Int32

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if r.Form.Get("grant_type") != "refresh_token" {
			http.Error(w, "want refresh_token", 400)
			return
		}
		if r.Form.Get("refresh_token") != "refresh-good" || r.Form.Get("client_id") != "cid-1" {
			http.Error(w, "bad refresh", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-fresh",
			"refresh_token": "refresh-good",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(tokenSrv.Close)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := upstreamHits.Add(1)
		auth := r.Header.Get("Authorization")
		switch {
		case auth == "Bearer access-fresh":
			io.WriteString(w, `{"ok":true}`)
		case auth == "Bearer access-expired":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://example/.well-known/oauth-protected-resource", error="invalid_token"`)
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
		default:
			t.Errorf("unexpected Authorization on hit %d: %q", n, auth)
			http.Error(w, "unexpected", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(up.Close)

	p, err := NewMCPProxy(&MCPProxyArgs{
		Prefix:  "/upstream",
		Servers: []MCPServer{{Name: "atlassian", URL: up.URL}},
		Probe: func(ctx context.Context, rawURL string) (*MCPProbe, error) {
			return &MCPProbe{Kind: MCPAuthOAuth, URL: rawURL, Status: 401, ResourceMetadata: "http://example/.well-known"}, nil
		},
		Authorize: func(ctx context.Context, args *AuthorizeMCPArgs) (*MCPToken, error) {
			authorizeHits.Add(1)
			t.Fatal("Authorize must not run when refresh succeeds")
			return nil, context.Canceled
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetToken("atlassian", &MCPToken{
		AccessToken:  "access-expired",
		RefreshToken: "refresh-good",
		TokenType:    "Bearer",
		ClientID:     "cid-1",
		TokenURL:     tokenSrv.URL,
		Resource:     up.URL,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upstream/atlassian", strings.NewReader(`{}`))
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := upstreamHits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (expired then fresh)", got)
	}
	if authorizeHits.Load() != 0 {
		t.Fatalf("Authorize called %d times", authorizeHits.Load())
	}
	tok := p.Token("atlassian")
	if tok == nil || tok.AccessToken != "access-fresh" {
		t.Fatalf("stored token = %+v", tok)
	}
}

// jevons 🎯T520: failed refresh is the only path that invokes the browser flow.
func TestMCPProxyFailedRefreshInvokesAuthorize(t *testing.T) {
	var authorizeHits atomic.Int32

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	t.Cleanup(tokenSrv.Close)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-from-browser" {
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
			authorizeHits.Add(1)
			return &MCPToken{AccessToken: "access-from-browser", TokenType: "Bearer"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetToken("atlassian", &MCPToken{
		AccessToken:  "access-expired",
		RefreshToken: "refresh-stale",
		ClientID:     "cid-1",
		TokenURL:     tokenSrv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upstream/atlassian", strings.NewReader(`{}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if authorizeHits.Load() != 1 {
		t.Fatalf("Authorize hits = %d, want 1 (failed refresh → browser)", authorizeHits.Load())
	}
}

func TestMCPProxyNoRefreshTokenInvokesAuthorize(t *testing.T) {
	var authorizeHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-xyz" {
			io.WriteString(w, `{"ok":true}`)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://example/.well-known"`)
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
			authorizeHits.Add(1)
			return &MCPToken{AccessToken: "access-xyz", TokenType: "Bearer"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetToken("atlassian", &MCPToken{AccessToken: "access-expired"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upstream/atlassian", strings.NewReader(`{}`)))
	if rec.Code != 200 || authorizeHits.Load() != 1 {
		t.Fatalf("status=%d authorize=%d body=%s", rec.Code, authorizeHits.Load(), rec.Body.String())
	}
}

// jevons 🎯T531: N concurrent 401s open one browser, not N.
func TestMCPProxyConcurrent401AuthorizesOnce(t *testing.T) {
	var authorizeHits atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	release := make(chan struct{})

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer once" {
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
			authorizeHits.Add(1)
			startOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &MCPToken{AccessToken: "once", TokenType: "Bearer"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upstream/atlassian", strings.NewReader(`{}`)))
			codes[i] = rec.Code
		}(i)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Authorize never started")
	}
	close(release)
	wg.Wait()
	if got := authorizeHits.Load(); got != 1 {
		t.Fatalf("Authorize called %d times, want 1", got)
	}
	for i, c := range codes {
		if c != 200 {
			t.Errorf("request %d status=%d, want 200", i, c)
		}
	}
}

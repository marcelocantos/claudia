// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// MCPProxyArgs configures [NewMCPProxy]. The host (jevonsd) mounts the
// handler and tells it how the outside world addresses it.
type MCPProxyArgs struct {
	// Prefix is the mount path the host routes to this handler
	// (e.g. "/upstream"). Incoming paths have this prefix stripped
	// before the server name is read. Empty means the host already
	// stripped it (http.StripPrefix).
	Prefix string
	// PublicBase is the advertised origin, e.g. "http://127.0.0.1:13705".
	// Combined with Prefix and the server name for [MCPProxy.Advertised].
	PublicBase string
	// Servers is the inventory to proxy. Entries without a URL (stdio)
	// are ignored.
	Servers []MCPServer
	// Client is used for upstream requests. Nil uses http.DefaultClient.
	Client *http.Client
	// Probe classifies an upstream. Nil uses [ProbeMCP].
	Probe func(ctx context.Context, rawURL string) (*MCPProbe, error)
	// Authorize runs owner-present OAuth. Nil uses [AuthorizeMCP].
	Authorize func(ctx context.Context, args *AuthorizeMCPArgs) (*MCPToken, error)
	// Refresh exchanges a refresh_token without a browser (jevons 🎯T520).
	// Nil uses [RefreshMCPToken].
	Refresh func(ctx context.Context, args *RefreshMCPArgs) (*MCPToken, error)
	// OpenURL is passed through to AuthorizeMCP. Tests inject a stub.
	OpenURL func(string) error
	// OnTokenChange is called after Authorize or Refresh stores a token.
	// The host uses it to persist tokens; Claudia does not.
	OnTokenChange func(name string, tok *MCPToken)
}

// MCPProxy is an http.Handler that reverse-proxies named HTTP MCP
// servers. A host mounts it and supplies Prefix + PublicBase (🎯T43).
// Tokens live in memory on the handler; Claudia does not persist them.
// On access-token expiry the proxy refreshes silently when a refresh
// token is present; Authorize (browser) runs only when there is no
// refresh token or refresh fails (jevons 🎯T520).
type MCPProxy struct {
	prefix        string
	publicBase    string
	client        *http.Client
	probe         func(context.Context, string) (*MCPProbe, error)
	authorize     func(context.Context, *AuthorizeMCPArgs) (*MCPToken, error)
	refresh       func(context.Context, *RefreshMCPArgs) (*MCPToken, error)
	openURL       func(string) error
	onTokenChange func(string, *MCPToken)

	mu     sync.Mutex
	byName map[string]*proxiedMCP
}

type proxiedMCP struct {
	srv   MCPServer
	probe *MCPProbe
	token *MCPToken

	// authMu serializes refresh/Authorize so concurrent 401s share one
	// browser tab (jevons 🎯T531).
	authMu sync.Mutex
}

// NewMCPProxy builds a handler for the HTTP entries in args.Servers.
func NewMCPProxy(args *MCPProxyArgs) (*MCPProxy, error) {
	if args == nil {
		return nil, fmt.Errorf("mcp proxy: args required")
	}
	p := &MCPProxy{
		prefix:        normalizePrefix(args.Prefix),
		publicBase:    strings.TrimRight(strings.TrimSpace(args.PublicBase), "/"),
		client:        args.Client,
		probe:         args.Probe,
		authorize:     args.Authorize,
		refresh:       args.Refresh,
		openURL:       args.OpenURL,
		onTokenChange: args.OnTokenChange,
		byName:        map[string]*proxiedMCP{},
	}
	if p.client == nil {
		p.client = http.DefaultClient
	}
	if p.probe == nil {
		p.probe = ProbeMCP
	}
	if p.authorize == nil {
		p.authorize = AuthorizeMCP
	}
	if p.refresh == nil {
		p.refresh = RefreshMCPToken
	}
	for _, s := range args.Servers {
		if strings.TrimSpace(s.URL) == "" || s.Name == "" {
			continue
		}
		cp := s
		p.byName[s.Name] = &proxiedMCP{srv: cp}
	}
	return p, nil
}

// Advertised returns HTTP MCPServer values whose URL is the public
// loopback path a Session backend should be given (PublicBase+Prefix+name).
func (p *MCPProxy) Advertised() []MCPServer {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []MCPServer
	for name, e := range p.byName {
		s := e.srv
		s.URL = p.publicURLLocked(name)
		s.Type = "http"
		s.Headers = nil
		s.BearerTokenEnv = ""
		s.Auth = ""
		out = append(out, s)
	}
	return out
}

// PublicURL is the advertised address of one proxied server.
func (p *MCPProxy) PublicURL(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publicURLLocked(name)
}

func (p *MCPProxy) publicURLLocked(name string) string {
	if p.publicBase == "" {
		return p.prefix + "/" + name
	}
	return p.publicBase + p.prefix + "/" + name
}

// SetToken seeds a stored token for name (host persistence → memory).
func (p *MCPProxy) SetToken(name string, tok *MCPToken) error {
	if name == "" || tok == nil {
		return fmt.Errorf("mcp proxy: set token requires name and token")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.byName[name]
	if !ok {
		return fmt.Errorf("mcp proxy: unknown server %q", name)
	}
	cp := *tok
	entry.token = &cp
	return nil
}

// Token returns a copy of the in-memory token for name, or nil.
func (p *MCPProxy) Token(name string) *MCPToken {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.byName[name]
	if !ok || entry.token == nil {
		return nil
	}
	cp := *entry.token
	return &cp
}

func (p *MCPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := p.serverName(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	entry, ok := p.byName[name]
	p.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	// Prefer sending known credentials on the first attempt so a stored
	// access token is exercised (and expiry can be detected) without an
	// always-unauthenticated probe round-trip (jevons 🎯T520).
	sent := p.bearer(entry)
	resp, err := p.forward(r.Context(), entry, r, body, sent != "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if err := p.ensureAuth(r.Context(), entry, resp, sent); err == nil {
			_ = resp.Body.Close()
			resp, err = p.forward(r.Context(), entry, r, body, true)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *MCPProxy) hasCreds(entry *proxiedMCP) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry.token != nil && entry.token.AccessToken != "" {
		return true
	}
	return len(entry.srv.Headers) > 0
}

func (p *MCPProxy) serverName(path string) (string, bool) {
	path = strings.TrimPrefix(path, p.prefix)
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", false
	}
	name, _, _ := strings.Cut(path, "/")
	if name == "" || strings.Contains(name, "..") {
		return "", false
	}
	return name, true
}

func (p *MCPProxy) forward(ctx context.Context, entry *proxiedMCP, in *http.Request, body []byte, withAuth bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, in.Method, entry.srv.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ct := in.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	if acc := in.Header.Get("Accept"); acc != "" {
		req.Header.Set("Accept", acc)
	} else {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	if v := in.Header.Get("MCP-Protocol-Version"); v != "" {
		req.Header.Set("MCP-Protocol-Version", v)
	}
	if v := in.Header.Get("MCP-Session-Id"); v != "" {
		req.Header.Set("MCP-Session-Id", v)
	}
	if withAuth {
		p.applyAuth(req, entry)
	}
	return p.client.Do(req)
}

func (p *MCPProxy) applyAuth(req *http.Request, entry *proxiedMCP) {
	p.mu.Lock()
	tok := entry.token
	hdrs := entry.srv.Headers
	envName := entry.srv.BearerTokenEnv
	p.mu.Unlock()
	if tok != nil && tok.AccessToken != "" {
		typ := tok.TokenType
		if typ == "" {
			typ = "Bearer"
		}
		req.Header.Set("Authorization", typ+" "+tok.AccessToken)
		return
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	if envName != "" && req.Header.Get("Authorization") == "" {
		// BearerTokenEnv is the Codex form; the process env is the host's.
		// We do not read the env here — the host should expand it into Headers.
	}
}

func (p *MCPProxy) bearer(entry *proxiedMCP) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry.token == nil || entry.token.AccessToken == "" {
		return ""
	}
	typ := entry.token.TokenType
	if typ == "" {
		typ = "Bearer"
	}
	return typ + " " + entry.token.AccessToken
}

func (p *MCPProxy) ensureAuth(ctx context.Context, entry *proxiedMCP, unauthorized *http.Response, sent string) error {
	// Probe cache is shared across concurrent 401s on the same entry.
	// Read and write under p.mu so TestMCPProxyConcurrent401AuthorizesOnce
	// (ENT-001 / T47.1) stays race-free; authMu alone does not cover this.
	p.mu.Lock()
	probe := entry.probe
	p.mu.Unlock()
	if probe == nil {
		var err error
		probe, err = p.probe(ctx, entry.srv.URL)
		if err != nil {
			// Fall back to the 401 we already have.
			www := unauthorized.Header.Get("WWW-Authenticate")
			meta := parseResourceMetadata(www)
			probe = &MCPProbe{
				URL:              entry.srv.URL,
				Status:           unauthorized.StatusCode,
				WWWAuthenticate:  www,
				ResourceMetadata: meta,
				Kind:             MCPAuthStatic,
			}
			if meta != "" {
				probe.Kind = MCPAuthOAuth
			}
		}
		p.mu.Lock()
		if entry.probe == nil {
			entry.probe = probe
		} else {
			probe = entry.probe
		}
		p.mu.Unlock()
	}

	switch probe.Kind {
	case MCPAuthOpen:
		return fmt.Errorf("mcp proxy: unexpected 401 from open server")
	case MCPAuthStatic:
		if len(entry.srv.Headers) == 0 {
			return fmt.Errorf("mcp proxy: static auth but no headers")
		}
		return nil
	case MCPAuthOAuth:
		return p.ensureOAuth(ctx, entry, probe, sent)
	default:
		return fmt.Errorf("mcp proxy: unknown auth kind %q", probe.Kind)
	}
}

// ensureOAuth refreshes when possible; Authorize (browser) only when
// there is no refresh token or refresh fails (jevons 🎯T520). Concurrent
// 401s that observed the same authGen share one Authorize (jevons 🎯T531).
func (p *MCPProxy) ensureOAuth(ctx context.Context, entry *proxiedMCP, probe *MCPProbe, sent string) error {
	entry.authMu.Lock()
	defer entry.authMu.Unlock()
	// Another 401 already refreshed/authorized; do not open a second tab.
	if now := p.bearer(entry); now != "" && now != sent {
		return nil
	}
	return p.runOAuth(ctx, entry, probe)
}

func (p *MCPProxy) runOAuth(ctx context.Context, entry *proxiedMCP, probe *MCPProbe) error {
	p.mu.Lock()
	cur := entry.token
	p.mu.Unlock()

	if cur != nil && strings.TrimSpace(cur.RefreshToken) != "" {
		next, err := p.refresh(ctx, &RefreshMCPArgs{Token: cur, Client: p.client})
		if err == nil {
			p.storeToken(entry, next)
			return nil
		}
		// Refresh failed — fall through to owner-present Authorize.
	}

	tok, err := p.authorize(ctx, &AuthorizeMCPArgs{
		URL:     entry.srv.URL,
		Probe:   probe,
		OpenURL: p.openURL,
		Client:  p.client,
	})
	if err != nil {
		return err
	}
	p.storeToken(entry, tok)
	return nil
}

func (p *MCPProxy) storeToken(entry *proxiedMCP, tok *MCPToken) {
	if tok == nil {
		return
	}
	cp := *tok
	p.mu.Lock()
	entry.token = &cp
	name := entry.srv.Name
	cb := p.onTokenChange
	p.mu.Unlock()
	if cb != nil {
		cb(name, &cp)
	}
}

func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if lk == "connection" || lk == "keep-alive" || lk == "transfer-encoding" || lk == "upgrade" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

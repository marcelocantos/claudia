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
	// OpenURL is passed through to AuthorizeMCP. Tests inject a stub.
	OpenURL func(string) error
}

// MCPProxy is an http.Handler that reverse-proxies named HTTP MCP
// servers. A host mounts it and supplies Prefix + PublicBase (🎯T43).
// Tokens live in memory on the handler; Claudia does not persist them.
type MCPProxy struct {
	prefix     string
	publicBase string
	client     *http.Client
	probe      func(context.Context, string) (*MCPProbe, error)
	authorize  func(context.Context, *AuthorizeMCPArgs) (*MCPToken, error)
	openURL    func(string) error

	mu     sync.Mutex
	byName map[string]*proxiedMCP
}

type proxiedMCP struct {
	srv   MCPServer
	probe *MCPProbe
	token *MCPToken
}

// NewMCPProxy builds a handler for the HTTP entries in args.Servers.
func NewMCPProxy(args *MCPProxyArgs) (*MCPProxy, error) {
	if args == nil {
		return nil, fmt.Errorf("mcp proxy: args required")
	}
	p := &MCPProxy{
		prefix:     normalizePrefix(args.Prefix),
		publicBase: strings.TrimRight(strings.TrimSpace(args.PublicBase), "/"),
		client:     args.Client,
		probe:      args.Probe,
		authorize:  args.Authorize,
		openURL:    args.OpenURL,
		byName:     map[string]*proxiedMCP{},
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

	resp, err := p.forward(r.Context(), entry, r, body, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if err := p.ensureAuth(r.Context(), entry, resp); err == nil {
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

func (p *MCPProxy) ensureAuth(ctx context.Context, entry *proxiedMCP, unauthorized *http.Response) error {
	p.mu.Lock()
	if entry.token != nil && entry.token.AccessToken != "" {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	probe := entry.probe
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
		entry.probe = probe
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
		tok, err := p.authorize(ctx, &AuthorizeMCPArgs{
			URL:     entry.srv.URL,
			Probe:   probe,
			OpenURL: p.openURL,
			Client:  p.client,
		})
		if err != nil {
			return err
		}
		p.mu.Lock()
		entry.token = tok
		p.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("mcp proxy: unknown auth kind %q", probe.Kind)
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

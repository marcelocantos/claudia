// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// mcpHost is the daemon's MCP connection owner (🎯T2.16). It listens on
// loopback, proxies HTTP remotes, keeps one stdio process per recipe, and
// rewrites a grant's MCPServers to those loopback URLs. Consumer servers
// named jevonsmcp* stay on the caller's URL.
type mcpHost struct {
	publicBase string
	addr       string
	ln         net.Listener
	srv        *http.Server
	proxy      *MCPProxy
	tokens     *mcpTokenStore
	upstreams  *mcpUpstreamStore
	log        func(string, ...any)

	mu     sync.Mutex
	stdio  map[string]*mcpStdioBackend
	recipe map[string]mcpRecipe
}

type mcpRecipe struct {
	kind    string // stdio | http
	command string
	args    string
	url     string
}

func (a mcpRecipe) equal(b mcpRecipe) bool {
	return a.kind == b.kind && a.command == b.command && a.args == b.args && a.url == b.url
}

type mcpHostOptions struct {
	StateDir   string
	ListenAddr string // default 127.0.0.1:0
	Logger     func(string, ...any)
}

func newMCPHost(opts mcpHostOptions) (*mcpHost, error) {
	addr := strings.TrimSpace(opts.ListenAddr)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mcp host: listen: %w", err)
	}
	bound := ln.Addr().String()
	host, port, err := net.SplitHostPort(bound)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("mcp host: addr: %w", err)
	}
	if host == "::" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	publicBase := "http://" + net.JoinHostPort(host, port)
	tokens, err := openMCPTokenStore(filepath.Join(opts.StateDir, "mcp_oauth_tokens.json"))
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	upstreams, err := openMCPUpstreamStore(filepath.Join(opts.StateDir, "mcp_upstreams.json"))
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	proxy, err := NewMCPProxy(&MCPProxyArgs{
		Prefix:        "/upstream",
		PublicBase:    publicBase,
		OnTokenChange: tokens.Put,
	})
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	for name, tok := range tokens.all() {
		_ = proxy.SetToken(name, &tok)
	}
	h := &mcpHost{
		publicBase: publicBase,
		addr:       net.JoinHostPort(host, port),
		ln:         ln,
		proxy:      proxy,
		tokens:     tokens,
		upstreams:  upstreams,
		log:        opts.Logger,
		stdio:      map[string]*mcpStdioBackend{},
		recipe:     map[string]mcpRecipe{},
	}
	if h.log == nil {
		h.log = func(string, ...any) {}
	}
	h.srv = &http.Server{Handler: h}
	go func() {
		if err := h.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			h.log("mcp host serve", "err", err)
		}
	}()
	if opts.StateDir != "" {
		_ = os.WriteFile(filepath.Join(opts.StateDir, "mcp.addr"), []byte(h.addr+"\n"), 0o644)
	}
	return h, nil
}

func (h *mcpHost) Addr() string { return h.addr }

func (h *mcpHost) Close() error {
	h.mu.Lock()
	for _, b := range h.stdio {
		b.close()
	}
	h.stdio = map[string]*mcpStdioBackend{}
	h.mu.Unlock()
	if h.srv != nil {
		return h.srv.Close()
	}
	if h.ln != nil {
		return h.ln.Close()
	}
	return nil
}

// Attach rewrites hosted owner-map servers to this process's loopback
// URLs. jevonsmcp* and a same-name different recipe are left alone.
func (h *mcpHost) Attach(servers []MCPServer) []MCPServer {
	if h == nil || len(servers) == 0 {
		return servers
	}
	out := make([]MCPServer, len(servers))
	for i, s := range servers {
		out[i] = s
		if url, ok := h.ensure(s); ok {
			out[i].URL = url
			out[i].Type = "http"
			out[i].Command = ""
			out[i].Args = nil
			out[i].Env = nil
			out[i].Headers = nil
			out[i].BearerTokenEnv = ""
			out[i].Auth = ""
		}
	}
	return out
}

func (h *mcpHost) ensure(s MCPServer) (string, bool) {
	if strings.TrimSpace(s.Name) == "" || consumerOwnedMCP(s) {
		return "", false
	}
	recipe, ok := recipeOf(s, h.upstreams)
	if !ok {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, have := h.recipe[s.Name]; have {
		if existing.equal(recipe) || h.isOurURLLocked(s.URL) {
			return h.publicURLLocked(s.Name), true
		}
		return "", false
	}
	if recipe.kind == "http" {
		backend := s
		backend.URL = recipe.url
		h.proxy.add(backend)
		if tok := h.tokens.Get(s.Name); tok != nil {
			_ = h.proxy.SetToken(s.Name, tok)
		}
		h.upstreams.remember(s.Name, recipe.url)
	} else {
		h.stdio[s.Name] = newMCPStdioBackend(s)
	}
	h.recipe[s.Name] = recipe
	return h.publicURLLocked(s.Name), true
}

func (h *mcpHost) publicURLLocked(name string) string {
	return h.publicBase + "/upstream/" + name
}

func (h *mcpHost) isOurURLLocked(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || h.publicBase == "" {
		return false
	}
	return strings.HasPrefix(raw, h.publicBase+"/upstream/")
}

func (h *mcpHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := mcpUpstreamName(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	stdio := h.stdio[name]
	h.mu.Unlock()
	if stdio != nil {
		stdio.ServeHTTP(w, r)
		return
	}
	h.proxy.ServeHTTP(w, r)
}

func mcpUpstreamName(path string) (string, bool) {
	path = strings.TrimPrefix(path, "/upstream/")
	if path == "" {
		return "", false
	}
	name, _, _ := strings.Cut(path, "/")
	if name == "" || strings.Contains(name, "..") {
		return "", false
	}
	return name, true
}

func consumerOwnedMCP(s MCPServer) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Name)), "jevonsmcp")
}

func recipeOf(s MCPServer, ups *mcpUpstreamStore) (mcpRecipe, bool) {
	if strings.TrimSpace(s.Command) != "" {
		return mcpRecipe{
			kind:    "stdio",
			command: s.Command,
			args:    strings.Join(s.Args, "\x00"),
		}, true
	}
	raw := strings.TrimSpace(s.URL)
	if raw == "" {
		return mcpRecipe{}, false
	}
	if name, ok := upstreamLoopbackName(raw); ok && name == s.Name && ups != nil {
		if real := ups.Get(s.Name); real != "" {
			raw = real
		}
	}
	return mcpRecipe{kind: "http", url: raw}, true
}

func upstreamLoopbackName(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	path := strings.TrimPrefix(u.Path, "/")
	head, rest, ok := strings.Cut(path, "/")
	if !ok || head != "upstream" || rest == "" {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return "", false
	}
	return name, true
}

func (p *MCPProxy) add(s MCPServer) {
	if strings.TrimSpace(s.URL) == "" || s.Name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := s
	if p.byName[s.Name] == nil {
		p.byName[s.Name] = &proxiedMCP{srv: cp}
		return
	}
	p.byName[s.Name].srv = cp
}

type mcpTokenStore struct {
	path string
	mu   sync.Mutex
	by   map[string]MCPToken
}

func openMCPTokenStore(path string) (*mcpTokenStore, error) {
	s := &mcpTokenStore{path: path, by: map[string]MCPToken{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	if len(s.by) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			_ = s.loadFile(filepath.Join(home, ".jevons", "mcp_oauth_tokens.json"))
		}
	}
	return s, nil
}

func (s *mcpTokenStore) load() error {
	return s.loadFile(s.path)
}

func (s *mcpTokenStore) loadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("mcp tokens: read: %w", err)
	}
	if len(b) == 0 {
		return nil
	}
	var doc map[string]MCPToken
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("mcp tokens: parse %s: %w", path, err)
	}
	for k, v := range doc {
		s.by[k] = v
	}
	return nil
}

func (s *mcpTokenStore) Get(name string) *MCPToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.by[name]
	if !ok {
		return nil
	}
	cp := tok
	return &cp
}

func (s *mcpTokenStore) all() map[string]MCPToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]MCPToken, len(s.by))
	for k, v := range s.by {
		out[k] = v
	}
	return out
}

func (s *mcpTokenStore) Put(name string, tok *MCPToken) {
	if name == "" || tok == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by[name] = *tok
	_ = s.flushLocked()
}

func (s *mcpTokenStore) flushLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.by, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

type mcpUpstreamStore struct {
	path string
	mu   sync.Mutex
	by   map[string]string
}

func openMCPUpstreamStore(path string) (*mcpUpstreamStore, error) {
	s := &mcpUpstreamStore{path: path, by: map[string]string{}}
	if err := s.loadFile(path); err != nil {
		return nil, err
	}
	if len(s.by) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			_ = s.loadFile(filepath.Join(home, ".jevons", "mcp_upstreams.json"))
		}
	}
	return s, nil
}

func (s *mcpUpstreamStore) loadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("mcp upstreams: read: %w", err)
	}
	if len(b) == 0 {
		return nil
	}
	var doc map[string]string
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("mcp upstreams: parse %s: %w", path, err)
	}
	for k, v := range doc {
		s.by[k] = v
	}
	return nil
}

func (s *mcpUpstreamStore) Get(name string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.by[name]
}

func (s *mcpUpstreamStore) remember(name, url string) {
	if s == nil || name == "" || url == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by[name] == url {
		return
	}
	s.by[name] = url
	if s.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	b, err := json.MarshalIndent(s.by, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	_ = os.WriteFile(tmp, append(b, '\n'), 0o600)
	_ = os.Rename(tmp, s.path)
}

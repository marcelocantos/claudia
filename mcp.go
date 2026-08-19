// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MCPServer is a provider-agnostic MCP registration (🎯T40). Callers
// name the server and transport; they do not write ~/.claude.json,
// ~/.grok/config.toml, or ~/.codex/config.toml themselves.
type MCPServer struct {
	Name    string            `json:"name"`
	Type    string            `json:"type,omitempty"` // "http" or "stdio"
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// Static HTTP auth (🎯T41). Headers is the Claude/Grok form
	// (Authorization: Bearer …, or ${ENV} interpolation). BearerTokenEnv
	// is the Codex form (bearer_token_env_var). Auth is Codex
	// oauth|chatgpt when no static token is supplied.
	Headers        map[string]string `json:"headers,omitempty"`
	HeadersHelper  string            `json:"headersHelper,omitempty"`
	BearerTokenEnv string            `json:"bearerTokenEnv,omitempty"`
	Auth           string            `json:"auth,omitempty"`

	// Providers is the discovery origin set (🎯T44). Empty means the
	// caller appended this server and it is valid for every Session
	// provider. A Codex-only computer-use entry has [ProviderCodex].
	Providers []Provider `json:"providers,omitempty"`
}

// LoadMCPArgs selects which on-disk configs to read (🎯T44). Nil is
// valid and loads all three user-scope defaults. If any path override
// is set, only those provided paths are read — tests must not leak
// daily ~/.grok or ~/.codex.
type LoadMCPArgs struct {
	ClaudeJSON string
	GrokTOML   string
	CodexTOML  string
	// WorkDir, when set, overlays Claude projects[WorkDir].mcpServers
	// on top of the Claude user-scope map.
	WorkDir string
}

// MCPInventory is the system MCP map in Claudia's dialect.
type MCPInventory struct {
	Servers []MCPServer
	Source  string   // first source, usually Claude JSON (compat)
	Sources []string // every file that was read
}

// EnsureMCPArgs is the single write surface (🎯T40). One name + HTTP URL
// is merged into each Session provider's own config file. Optional
// Headers / BearerTokenEnv / Auth travel with the registration (🎯T41).
type EnsureMCPArgs struct {
	Name           string
	URL            string
	Headers        map[string]string
	HeadersHelper  string
	BearerTokenEnv string
	Auth           string
	// Path overrides. Empty uses the production user-scope files.
	// Isolates and tests must pass fixture paths — never the daily
	// ~/.claude.json / ~/.grok/config.toml / ~/.codex/config.toml.
	ClaudeJSON string
	GrokTOML   string
	CodexTOML  string
	// Providers limits which backends to write. Empty means Claude,
	// Grok, and Codex. Bedrock and Ollama have no MCP ensure path.
	Providers []Provider
}

var mcpServerNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// LoadMCP reads each Session provider's own MCP config and tags
// servers with their origin (🎯T44). A host that wants "what's already
// on the system" plus its own server does:
//
//	inv, err := claudia.LoadMCP(nil)
//	inv.Servers = append(inv.Servers, claudia.MCPServer{Name: "jevonsmcp", Type: "http", URL: url})
//	cfg.MCPServers = inv.ForProvider(cfg.Provider)
func LoadMCP(args *LoadMCPArgs) (*MCPInventory, error) {
	if args == nil {
		args = &LoadMCPArgs{}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("load mcp: home dir: %w", err)
	}
	type src struct {
		path     string
		provider Provider
		kind     string
	}
	var sources []src
	explicit := args.ClaudeJSON != "" || args.GrokTOML != "" || args.CodexTOML != ""
	if !explicit || args.ClaudeJSON != "" {
		path := args.ClaudeJSON
		if path == "" {
			path = filepath.Join(home, ".claude.json")
		}
		sources = append(sources, src{path, ProviderClaude, "claude"})
	}
	if !explicit || args.GrokTOML != "" {
		path := args.GrokTOML
		if path == "" {
			path = filepath.Join(home, ".grok", "config.toml")
		}
		sources = append(sources, src{path, ProviderGrok, "grok"})
	}
	if !explicit || args.CodexTOML != "" {
		path := args.CodexTOML
		if path == "" {
			path = filepath.Join(home, ".codex", "config.toml")
		}
		sources = append(sources, src{path, ProviderCodex, "codex"})
	}

	var servers []MCPServer
	var read []string
	for _, s := range sources {
		var got []MCPServer
		var err error
		switch s.kind {
		case "claude":
			got, err = readClaudeMCPMap(s.path, args.WorkDir)
		default:
			got, err = readTOMLMCPMap(s.path)
		}
		if err != nil {
			return nil, err
		}
		read = append(read, s.path)
		for i := range got {
			got[i].Providers = []Provider{s.provider}
		}
		servers = overlayMCPServers(servers, got)
	}
	source := ""
	if len(read) > 0 {
		source = read[0]
	}
	return &MCPInventory{Servers: servers, Source: source, Sources: read}, nil
}

// ForProvider returns servers that belong on this Session provider.
// Caller-appended entries with empty Providers are included for every
// provider.
func (inv *MCPInventory) ForProvider(p Provider) []MCPServer {
	if inv == nil {
		return nil
	}
	var out []MCPServer
	for _, s := range inv.Servers {
		if len(s.Providers) == 0 || hasProvider(s.Providers, p) {
			out = append(out, s)
		}
	}
	return out
}

// EnsureMCP merges an HTTP MCP server into each requested provider's
// native config. Missing or stale entries are corrected. A second call
// with the same name and URL is a no-op. Files are flocked so concurrent
// ensurers do not clobber each other.
func EnsureMCP(args *EnsureMCPArgs) error {
	if args == nil {
		return fmt.Errorf("ensure mcp: args required")
	}
	name := strings.TrimSpace(args.Name)
	url := strings.TrimSpace(args.URL)
	if !mcpServerNameRE.MatchString(name) {
		return fmt.Errorf("ensure mcp: invalid server name %q", args.Name)
	}
	if url == "" {
		return fmt.Errorf("ensure mcp: url required")
	}
	providers := args.Providers
	if len(providers) == 0 {
		providers = []Provider{ProviderClaude, ProviderGrok, ProviderCodex}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("ensure mcp: home dir: %w", err)
	}
	for _, p := range providers {
		switch p {
		case ProviderClaude:
			path := args.ClaudeJSON
			if path == "" {
				path = filepath.Join(home, ".claude.json")
			}
			if _, err := upsertClaudeJSON(path, args.server()); err != nil {
				return fmt.Errorf("ensure mcp claude: %w", err)
			}
		case ProviderGrok:
			path := args.GrokTOML
			if path == "" {
				path = filepath.Join(home, ".grok", "config.toml")
			}
			if _, err := upsertTOMLHTTPServer(path, args.server()); err != nil {
				return fmt.Errorf("ensure mcp grok: %w", err)
			}
		case ProviderCodex:
			path := args.CodexTOML
			if path == "" {
				path = filepath.Join(home, ".codex", "config.toml")
			}
			if _, err := upsertTOMLHTTPServer(path, args.server()); err != nil {
				return fmt.Errorf("ensure mcp codex: %w", err)
			}
		case ProviderBedrock, ProviderOllama:
			// No Session MCP surface. Callers skip these explicitly.
			continue
		default:
			return fmt.Errorf("ensure mcp: unknown provider %q", p)
		}
	}
	return nil
}

func (a *EnsureMCPArgs) server() MCPServer {
	return MCPServer{
		Name:           strings.TrimSpace(a.Name),
		Type:           "http",
		URL:            strings.TrimSpace(a.URL),
		Headers:        a.Headers,
		HeadersHelper:  strings.TrimSpace(a.HeadersHelper),
		BearerTokenEnv: strings.TrimSpace(a.BearerTokenEnv),
		Auth:           strings.TrimSpace(a.Auth),
	}
}

func claudiaMCPFile(workDir string) string {
	return filepath.Join(workDir, "mcp.claudia.json")
}

func claudeMCPConfigArg(req agentStartRequest) string {
	if len(req.Config.MCPServers) > 0 {
		return claudiaMCPFile(req.WorkDir)
	}
	return req.Config.MCPConfig
}

func writeSessionMCPFile(req agentStartRequest) error {
	if len(req.Config.MCPServers) == 0 {
		return nil
	}
	path := claudiaMCPFile(req.WorkDir)
	return writeClaudeMCPJSON(path, mergeMCPServers(req.Config))
}

func mergeMCPServers(cfg Config) []MCPServer {
	var out []MCPServer
	if cfg.MCPConfig != "" {
		if loaded, err := readClaudeMCPMap(cfg.MCPConfig, ""); err == nil {
			out = append(out, loaded...)
		}
	}
	return overlayMCPServers(out, cfg.MCPServers)
}

func resolveACPMCPServers(cfg Config) []any {
	servers := mergeMCPServers(cfg)
	if len(servers) == 0 {
		return acpMCPServers(cfg.MCPConfig)
	}
	return mcpServersToACP(servers)
}

func overlayMCPServers(base, extra []MCPServer) []MCPServer {
	if len(extra) == 0 {
		return base
	}
	byName := map[string]int{}
	out := append([]MCPServer(nil), base...)
	for i, s := range out {
		byName[s.Name] = i
	}
	for _, s := range extra {
		if i, ok := byName[s.Name]; ok {
			prev := out[i].Providers
			out[i] = s
			out[i].Providers = unionProviders(prev, s.Providers)
			continue
		}
		byName[s.Name] = len(out)
		out = append(out, s)
	}
	return out
}

func hasProvider(ps []Provider, p Provider) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

func unionProviders(a, b []Provider) []Provider {
	out := append([]Provider(nil), a...)
	for _, p := range b {
		if !hasProvider(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func mcpServersToACP(servers []MCPServer) []any {
	var out []any
	for _, s := range servers {
		switch {
		case strings.TrimSpace(s.URL) != "":
			entry := map[string]any{
				"type":    "http",
				"name":    s.Name,
				"url":     s.URL,
				"headers": acpHeaders(s),
			}
			out = append(out, entry)
		case s.Command != "":
			envs := []any{}
			for k, v := range s.Env {
				envs = append(envs, map[string]any{"name": k, "value": v})
			}
			args := s.Args
			if args == nil {
				args = []string{}
			}
			out = append(out, map[string]any{
				"name":    s.Name,
				"command": s.Command,
				"args":    args,
				"env":     envs,
			})
		}
	}
	return out
}

func readClaudeMCPMap(path, workDir string) ([]MCPServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mcp %s: %w", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse mcp %s: %w", path, err)
	}
	servers := mcpMapFromAny(root["mcpServers"])
	if workDir != "" {
		if projects, ok := root["projects"].(map[string]any); ok {
			if proj, ok := projects[workDir].(map[string]any); ok {
				servers = overlayMCPServers(servers, mcpMapFromAny(proj["mcpServers"]))
			}
		}
	}
	return servers, nil
}

func mcpMapFromAny(v any) []MCPServer {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var out []MCPServer
	for name, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		s := MCPServer{Name: name, Type: stringField(m, "type")}
		s.URL = stringField(m, "url")
		s.Command = stringField(m, "command")
		s.HeadersHelper = stringField(m, "headersHelper")
		s.BearerTokenEnv = stringField(m, "bearerTokenEnv")
		if s.BearerTokenEnv == "" {
			s.BearerTokenEnv = stringField(m, "bearer_token_env_var")
		}
		s.Auth = stringField(m, "auth")
		s.Headers = stringMapField(m, "headers")
		if args, ok := m["args"].([]any); ok {
			for _, a := range args {
				if str, ok := a.(string); ok {
					s.Args = append(s.Args, str)
				}
			}
		}
		if env, ok := m["env"].(map[string]any); ok {
			s.Env = map[string]string{}
			for k, val := range env {
				if str, ok := val.(string); ok {
					s.Env[k] = str
				}
			}
		}
		if s.Type == "" {
			if s.URL != "" {
				s.Type = "http"
			} else if s.Command != "" {
				s.Type = "stdio"
			}
		}
		out = append(out, s)
	}
	return out
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func stringMapField(m map[string]any, key string) map[string]string {
	raw, ok := m[key].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		if str, ok := v.(string); ok && str != "" {
			out[k] = str
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func acpHeaders(s MCPServer) []any {
	// Grok ACP session/new requires `headers` as an array (empty is
	// fine). Omitting the field is Invalid params (live 2026-08-19).
	out := []any{}
	for k, v := range s.Headers {
		out = append(out, map[string]any{"name": k, "value": v})
	}
	return out
}

func writeClaudeMCPJSON(path string, servers []MCPServer) error {
	root := map[string]any{"mcpServers": claudeMCPObject(servers)}
	return writeJSONFile(path, root)
}

func claudeMCPObject(servers []MCPServer) map[string]any {
	obj := map[string]any{}
	for _, s := range servers {
		entry := map[string]any{}
		if s.Type != "" {
			entry["type"] = s.Type
		}
		if s.URL != "" {
			entry["type"] = "http"
			entry["url"] = s.URL
		}
		if len(s.Headers) > 0 {
			entry["headers"] = s.Headers
		}
		if s.HeadersHelper != "" {
			entry["headersHelper"] = s.HeadersHelper
		}
		if s.BearerTokenEnv != "" {
			entry["bearerTokenEnv"] = s.BearerTokenEnv
		}
		if s.Auth != "" {
			entry["auth"] = s.Auth
		}
		if s.Command != "" {
			entry["command"] = s.Command
			if len(s.Args) > 0 {
				entry["args"] = s.Args
			}
			if len(s.Env) > 0 {
				entry["env"] = s.Env
			}
		}
		obj[s.Name] = entry
	}
	return obj
}

func upsertClaudeJSON(path string, srv MCPServer) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := flockExclusive(f); err != nil {
		return false, err
	}
	defer flockUnlock(f)

	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	want := claudeHTTPEntry(srv)
	if existing, ok := servers[srv.Name].(map[string]any); ok && sameStringAnyMap(existing, want) {
		return false, nil
	}
	servers[srv.Name] = want
	root["mcpServers"] = servers
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if err := f.Truncate(0); err != nil {
		return false, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return false, err
	}
	if _, err := f.Write(out); err != nil {
		return false, err
	}
	return true, nil
}

func writeJSONFile(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

func claudeHTTPEntry(srv MCPServer) map[string]any {
	entry := map[string]any{"type": "http", "url": srv.URL}
	if len(srv.Headers) > 0 {
		entry["headers"] = srv.Headers
	}
	if srv.HeadersHelper != "" {
		entry["headersHelper"] = srv.HeadersHelper
	}
	if srv.BearerTokenEnv != "" {
		entry["bearerTokenEnv"] = srv.BearerTokenEnv
	}
	if srv.Auth != "" {
		entry["auth"] = srv.Auth
	}
	return entry
}

func sameStringAnyMap(a, b map[string]any) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(aj) == string(bj)
}

func upsertTOMLHTTPServer(path string, srv MCPServer) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := flockExclusive(f); err != nil {
		return false, err
	}
	defer flockUnlock(f)

	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	next, changed := mergeTOMLHTTPServer(string(data), srv)
	if !changed {
		return false, nil
	}
	if err := f.Truncate(0); err != nil {
		return false, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return false, err
	}
	if _, err := f.Write([]byte(next)); err != nil {
		return false, err
	}
	return true, nil
}

func mergeTOMLHTTPServer(src string, srv MCPServer) (string, bool) {
	prefix := "mcp_servers." + srv.Name
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var keep []string
	skip := false
	had := false
	cur := map[string]string{}
	curHeaders := map[string]string{}
	inHeaders := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if key, ok := tomlTableKey(trim); ok {
			if key == prefix || strings.HasPrefix(key, prefix+".") {
				skip = true
				inHeaders = key == prefix+".headers"
				if key == prefix {
					had = true
				}
				continue
			}
			skip = false
			inHeaders = false
		}
		if skip {
			if k, v, ok := tomlBareKV(trim); ok {
				v = strings.Trim(v, `"'`)
				if inHeaders {
					curHeaders[k] = v
				} else {
					cur[k] = v
				}
			}
			continue
		}
		keep = append(keep, line)
	}
	if had && cur["url"] == srv.URL &&
		cur["bearer_token_env_var"] == srv.BearerTokenEnv &&
		cur["auth"] == srv.Auth &&
		sameStringMap(curHeaders, srv.Headers) {
		return src, false
	}
	for len(keep) > 0 && strings.TrimSpace(keep[len(keep)-1]) == "" {
		keep = keep[:len(keep)-1]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n[mcp_servers.%s]\nurl = %q\nenabled = true\n", srv.Name, srv.URL)
	if srv.BearerTokenEnv != "" {
		fmt.Fprintf(&b, "bearer_token_env_var = %q\n", srv.BearerTokenEnv)
	}
	if srv.Auth != "" {
		fmt.Fprintf(&b, "auth = %q\n", srv.Auth)
	}
	if len(srv.Headers) > 0 {
		fmt.Fprintf(&b, "\n[mcp_servers.%s.headers]\n", srv.Name)
		for k, v := range srv.Headers {
			fmt.Fprintf(&b, "%s = %q\n", k, v)
		}
	}
	block := b.String()
	if len(keep) == 0 {
		return strings.TrimPrefix(block, "\n"), true
	}
	return strings.Join(keep, "\n") + "\n" + block, true
}

func readTOMLMCPMap(path string) ([]MCPServer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mcp %s: %w", path, err)
	}
	type acc struct {
		srv     MCPServer
		enabled bool
		have    bool
	}
	byName := map[string]*acc{}
	order := []string{}
	cur := ""
	sub := ""
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		trim := strings.TrimSpace(lines[i])
		if key, ok := tomlTableKey(trim); ok {
			name, rest, ok := tomlMCPServerKey(key)
			if !ok {
				cur, sub = "", ""
				continue
			}
			if _, exists := byName[name]; !exists {
				byName[name] = &acc{srv: MCPServer{Name: name}, enabled: true}
				order = append(order, name)
			}
			cur, sub = name, rest
			continue
		}
		if cur == "" {
			continue
		}
		k, v, ok := tomlBareKV(trim)
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		a := byName[cur]
		a.have = true
		switch {
		case sub == "headers":
			if a.srv.Headers == nil {
				a.srv.Headers = map[string]string{}
			}
			a.srv.Headers[k] = v
		case sub == "env":
			if a.srv.Env == nil {
				a.srv.Env = map[string]string{}
			}
			a.srv.Env[k] = v
		case sub != "":
			// tools.* and other nested tables are ignored
		case k == "url":
			a.srv.URL = v
		case k == "command":
			a.srv.Command = v
		case k == "args":
			a.srv.Args = parseTOMLStringArray(v)
		case k == "bearer_token_env_var":
			a.srv.BearerTokenEnv = v
		case k == "auth":
			a.srv.Auth = v
		case k == "enabled":
			a.enabled = v != "false"
		}
	}
	var out []MCPServer
	for _, name := range order {
		a := byName[name]
		if !a.have || !a.enabled {
			continue
		}
		if a.srv.Type == "" {
			if a.srv.URL != "" {
				a.srv.Type = "http"
			} else if a.srv.Command != "" {
				a.srv.Type = "stdio"
			}
		}
		out = append(out, a.srv)
	}
	return out, nil
}

func tomlMCPServerKey(key string) (name, sub string, ok bool) {
	const prefix = "mcp_servers."
	if !strings.HasPrefix(key, prefix) {
		return "", "", false
	}
	rest := key[len(prefix):]
	if rest == "" {
		return "", "", false
	}
	name, after, found := strings.Cut(rest, ".")
	if !mcpServerNameRE.MatchString(name) {
		return "", "", false
	}
	if found {
		return name, after, true
	}
	return name, "", true
}

func parseTOMLStringArray(v string) []string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") {
		if v == "" {
			return nil
		}
		return []string{strings.Trim(v, `"'`)}
	}
	v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(strings.Trim(part, `"'`))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func tomlTableKey(trim string) (string, bool) {
	if !strings.HasPrefix(trim, "[") || !strings.HasSuffix(trim, "]") {
		return "", false
	}
	if strings.HasPrefix(trim, "[[") {
		return "", false
	}
	return strings.TrimSpace(trim[1 : len(trim)-1]), true
}

func tomlBareKV(trim string) (k, v string, ok bool) {
	if trim == "" || strings.HasPrefix(trim, "#") {
		return "", "", false
	}
	i := strings.IndexByte(trim, '=')
	if i < 0 {
		return "", "", false
	}
	k = strings.TrimSpace(trim[:i])
	v = strings.TrimSpace(trim[i+1:])
	if k == "" {
		return "", "", false
	}
	return k, v, true
}

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
}

// LoadMCPArgs selects which on-disk Claude config to read. Nil is valid
// (defaults). Claude user-scope is the default system map so a host can
// say "use what's already on the machine" without knowing the file.
type LoadMCPArgs struct {
	// ClaudeJSON overrides ~/.claude.json.
	ClaudeJSON string
	// WorkDir, when set, overlays projects[WorkDir].mcpServers on top
	// of the user-scope map (same directory-key rule Claude uses).
	WorkDir string
}

// MCPInventory is the system MCP map in Claudia's dialect.
type MCPInventory struct {
	Servers []MCPServer
	Source  string
}

// EnsureMCPArgs is the single write surface (🎯T40). One name + HTTP URL
// is merged into each Session provider's own config file.
type EnsureMCPArgs struct {
	Name string
	URL  string
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

// LoadMCP reads the current Claude user-scope MCP map (and optional
// project overlay) into provider-agnostic [MCPServer] values. A host
// that wants "what's already on the system" plus its own server does:
//
//	inv, err := claudia.LoadMCP(nil)
//	inv.Servers = append(inv.Servers, claudia.MCPServer{Name: "jevonsmcp", Type: "http", URL: url})
//	cfg.MCPServers = inv.Servers
func LoadMCP(args *LoadMCPArgs) (*MCPInventory, error) {
	if args == nil {
		args = &LoadMCPArgs{}
	}
	path := args.ClaudeJSON
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("load mcp: home dir: %w", err)
		}
		path = filepath.Join(home, ".claude.json")
	}
	servers, err := readClaudeMCPMap(path, args.WorkDir)
	if err != nil {
		return nil, err
	}
	return &MCPInventory{Servers: servers, Source: path}, nil
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
			if _, err := upsertClaudeJSON(path, name, url); err != nil {
				return fmt.Errorf("ensure mcp claude: %w", err)
			}
		case ProviderGrok:
			path := args.GrokTOML
			if path == "" {
				path = filepath.Join(home, ".grok", "config.toml")
			}
			if _, err := upsertTOMLHTTPServer(path, name, url); err != nil {
				return fmt.Errorf("ensure mcp grok: %w", err)
			}
		case ProviderCodex:
			path := args.CodexTOML
			if path == "" {
				path = filepath.Join(home, ".codex", "config.toml")
			}
			if _, err := upsertTOMLHTTPServer(path, name, url); err != nil {
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
			out[i] = s
			continue
		}
		byName[s.Name] = len(out)
		out = append(out, s)
	}
	return out
}

func mcpServersToACP(servers []MCPServer) []any {
	var out []any
	for _, s := range servers {
		switch {
		case strings.TrimSpace(s.URL) != "":
			out = append(out, map[string]any{
				"type":    "http",
				"name":    s.Name,
				"url":     s.URL,
				"headers": []any{},
			})
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

func upsertClaudeJSON(path, name, url string) (bool, error) {
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
	if existing, ok := servers[name].(map[string]any); ok {
		if stringField(existing, "url") == url && (stringField(existing, "type") == "http" || stringField(existing, "type") == "") {
			return false, nil
		}
	}
	servers[name] = map[string]any{"type": "http", "url": url}
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

func upsertTOMLHTTPServer(path, name, url string) (bool, error) {
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
	next, changed := mergeTOMLHTTPServer(string(data), name, url)
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

func mergeTOMLHTTPServer(src, name, url string) (string, bool) {
	prefix := "mcp_servers." + name
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var keep []string
	skip := false
	had := false
	curURL := ""
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if key, ok := tomlTableKey(trim); ok {
			if key == prefix || strings.HasPrefix(key, prefix+".") {
				skip = true
				if key == prefix {
					had = true
				}
				continue
			}
			skip = false
		}
		if skip {
			if k, v, ok := tomlBareKV(trim); ok && k == "url" {
				curURL = strings.Trim(v, `"'`)
			}
			continue
		}
		keep = append(keep, line)
	}
	if had && curURL == url {
		return src, false
	}
	for len(keep) > 0 && strings.TrimSpace(keep[len(keep)-1]) == "" {
		keep = keep[:len(keep)-1]
	}
	block := fmt.Sprintf("\n[mcp_servers.%s]\nurl = %q\nenabled = true\n", name, url)
	if len(keep) == 0 {
		return strings.TrimPrefix(block, "\n"), true
	}
	return strings.Join(keep, "\n") + "\n" + block, true
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

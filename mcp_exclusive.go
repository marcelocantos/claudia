// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// prepareExclusiveGrokHome builds a process-private GROK_HOME under the
// system temp dir (never WorkDir, never ~/.grok). Auth is copied read-only
// from the user home when present. Compat Claude/Cursor MCP discovery is
// disabled so Config.MCPServers on the ACP wire is the only MCP set.
func prepareExclusiveGrokHome() (home string, cleanup func(), err error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	dest, err := os.MkdirTemp("", "claudia-mcp-grok-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dest) }
	_ = copyFileIfExists(filepath.Join(userHome, ".grok", "auth.json"), filepath.Join(dest, "auth.json"))
	cfg := filepath.Join(dest, "config.toml")
	// Grok loads ~/.claude.json MCP by default ([compat.claude] mcps).
	// Exclusive must disable that discovery.
	body := "# claudia MCPExclusive\n" +
		"[compat.claude]\nmcps = false\n\n" +
		"[compat.cursor]\nmcps = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		cleanup()
		return "", nil, err
	}
	return dest, cleanup, nil
}

// prepareExclusiveCodexHome builds a process-private CODEX_HOME under the
// system temp dir and writes only the named HTTP MCP servers into its
// config.toml. Never mutates ~/.codex/config.toml or the project tree.
// Auth is copied read-only from the user home when present.
func prepareExclusiveCodexHome(servers []MCPServer) (home string, cleanup func(), err error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	dest, err := os.MkdirTemp("", "claudia-mcp-codex-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dest) }
	_ = copyFileIfExists(filepath.Join(userHome, ".codex", "auth.json"), filepath.Join(dest, "auth.json"))
	cfg := filepath.Join(dest, "config.toml")
	if err := os.WriteFile(cfg, []byte("# claudia session MCP\n"), 0o644); err != nil {
		cleanup()
		return "", nil, err
	}
	for _, s := range httpMCPServers(servers) {
		if _, err := upsertTOMLHTTPServer(cfg, s); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("exclusive codex mcp %s: %w", s.Name, err)
		}
	}
	return dest, cleanup, nil
}

func httpMCPServers(servers []MCPServer) []MCPServer {
	var out []MCPServer
	for _, s := range servers {
		if s.URL == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func exclusiveEnv(key, value string) []string {
	if value == "" {
		return nil
	}
	return []string{key + "=" + value}
}

func appendEnv(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	if base == nil {
		base = os.Environ()
	}
	override := map[string]struct{}{}
	for _, e := range extra {
		k, _, _ := strings.Cut(e, "=")
		override[k] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := override[k]; ok {
			continue
		}
		out = append(out, e)
	}
	return append(out, extra...)
}

func copyFileIfExists(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func joinCleanups(fns ...func()) func() {
	return func() {
		for i := len(fns) - 1; i >= 0; i-- {
			if fns[i] != nil {
				fns[i]()
			}
		}
	}
}

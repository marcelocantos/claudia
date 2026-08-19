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

func exclusiveMCPHomeDir(workDir, kind string) string {
	if workDir == "" {
		workDir = os.TempDir()
	}
	return filepath.Join(workDir, ".claudia-mcp-home", kind)
}

func prepareExclusiveGrokHome(workDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dest := exclusiveMCPHomeDir(workDir, "grok")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	_ = copyFileIfExists(filepath.Join(home, ".grok", "auth.json"), filepath.Join(dest, "auth.json"))
	cfg := filepath.Join(dest, "config.toml")
	// Grok loads ~/.claude.json MCP by default ([compat.claude] mcps).
	// An empty GROK_HOME config *enables* that discovery; daily
	// ~/.grok/config.toml already sets mcps=false. Exclusive must too.
	body := "# claudia MCPExclusive\n" +
		"[compat.claude]\nmcps = false\n\n" +
		"[compat.cursor]\nmcps = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

func prepareExclusiveCodexHome(workDir string, servers []MCPServer) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dest := exclusiveMCPHomeDir(workDir, "codex")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	_ = copyFileIfExists(filepath.Join(home, ".codex", "auth.json"), filepath.Join(dest, "auth.json"))
	cfg := filepath.Join(dest, "config.toml")
	if err := os.WriteFile(cfg, []byte("# claudia MCPExclusive\n"), 0o644); err != nil {
		return "", err
	}
	for _, s := range servers {
		if s.URL == "" {
			continue
		}
		if _, err := upsertTOMLHTTPServer(cfg, s); err != nil {
			return "", fmt.Errorf("exclusive codex mcp %s: %w", s.Name, err)
		}
	}
	return dest, nil
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

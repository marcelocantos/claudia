// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// prepareExclusiveGrokHome builds a process-private GROK_HOME under the
// system temp dir (never WorkDir, never ~/.grok). Auth is copied read-only
// from the user home when present. Compat Claude/Cursor MCP discovery is
// disabled so Config.MCPServers on the ACP wire is the only MCP set.
func prepareExclusiveGrokHome() (home string, cleanup func(), err error) {
	dest, err := os.MkdirTemp("", "claudia-mcp-grok-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dest) }
	if err := writeExclusiveGrokHome(dest); err != nil {
		cleanup()
		return "", nil, err
	}
	return dest, cleanup, nil
}

func writeExclusiveGrokHome(dest string) error {
	// Validate both leaves even when auth comes from the environment and
	// there is no user auth file to copy. The provider still reads this path.
	for _, name := range []string{"auth.json", "config.toml"} {
		path := filepath.Join(dest, name)
		if info, err := os.Lstat(path); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("exclusive GROK_HOME configuration is not a regular file: %s", path)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Missing file auth is valid when the provider uses its environment or
	// keychain. Other copy errors must not silently produce a broken home.
	auth, err := os.ReadFile(filepath.Join(userHome, ".grok", "auth.json"))
	if err == nil {
		if err := writeExclusiveGrokFile(dest, "auth.json", auth); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	body := "# claudia MCPExclusive\n" +
		"[compat.claude]\nmcps = false\n\n" +
		"[compat.cursor]\nmcps = false\n"
	return writeExclusiveGrokFile(dest, "config.toml", []byte(body))
}

// exclusiveCodexHomeDir is the durable isolate home for a Codex thread.
// Bounce resume (jevons 🎯T545.1.2) looks here — a temp dir deleted on
// Stop cannot hold the rollout that thread/resume needs.
func exclusiveCodexHomeDir(sessionID string) string {
	sid := exclusiveCodexHomeKey(sessionID)
	if sid == "" {
		return ""
	}
	return filepath.Join(claudiaStateHome(), "codex-homes", sid)
}

func exclusiveCodexHomeKey(sessionID string) string {
	sid := strings.TrimSpace(sessionID)
	if sid == "" || sid == "." || sid == ".." || strings.ContainsAny(sid, `/\`) {
		return ""
	}
	return sid
}

func claudiaStateHome() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(stateHome, "claudia")
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// exclusiveCodexHomeForStart reuses the durable isolate home when the
// session already has one. A first mint with a known session id creates
// that durable dir immediately — jevons SIGHUP skips StopAll, so a
// temp home deleted (or never persisted) on coordinator death cannot
// hold the rollout. RequireResume + a missing home is fail-loud and
// names the path.
func exclusiveCodexHomeForStart(sessionID string, requireResume bool, servers []MCPServer, sandbox codexSandboxTuning) (home string, cleanup func(), err error) {
	dest := exclusiveCodexHomeDir(sessionID)
	if dest != "" {
		if dirExists(dest) {
			if err := writeExclusiveCodexHome(dest, servers, sandbox); err != nil {
				return "", nil, err
			}
			return dest, func() {}, nil
		}
		if requireResume {
			return "", nil, fmt.Errorf("thread/resume: exclusive CODEX_HOME missing at %s — refusing to mint", dest)
		}
		if err := writeExclusiveCodexHome(dest, servers, sandbox); err != nil {
			return "", nil, err
		}
		return dest, func() {}, nil
	}
	return prepareExclusiveCodexHome(servers, sandbox)
}

// publishExclusiveCodexHome makes dest(sessionID) resolve to src without
// moving src (the running app-server still has CODEX_HOME=src). Durable
// src becomes a symlink; a temp src is copied. Call this as soon as
// thread/start returns — do not wait for Stop.
func publishExclusiveCodexHome(src, sessionID string) error {
	dest := exclusiveCodexHomeDir(sessionID)
	if src == "" || dest == "" {
		return fmt.Errorf("publish exclusive CODEX_HOME: empty src or session")
	}
	if filepath.Clean(src) == filepath.Clean(dest) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if dirExists(dest) {
		return nil
	}
	if isDurableCodexHome(src) {
		if err := os.Symlink(src, dest); err == nil {
			return nil
		}
	}
	return copyDir(src, dest)
}

func isDurableCodexHome(path string) bool {
	root := filepath.Join(claudiaStateHome(), "codex-homes")
	rel, err := filepath.Rel(root, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "..")
}

func exclusiveCodexHomeCleanup(home, sessionID string, ephemeral func()) func() {
	return func() {
		if sessionID != "" {
			if err := publishExclusiveCodexHome(home, sessionID); err != nil {
				dest := exclusiveCodexHomeDir(sessionID)
				slog.Error("persist exclusive CODEX_HOME", "src", home, "dest", dest, "err", err)
			}
		}
		if isDurableCodexHome(home) {
			return
		}
		if ephemeral != nil {
			ephemeral()
		}
	}
}

func persistExclusiveCodexHome(src, sessionID string) error {
	dest := exclusiveCodexHomeDir(sessionID)
	if src == "" || dest == "" {
		return fmt.Errorf("persist exclusive CODEX_HOME: empty src or session")
	}
	if filepath.Clean(src) == filepath.Clean(dest) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if dirExists(dest) {
		if src != dest {
			_ = os.RemoveAll(src)
		}
		return nil
	}
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	if err := copyDir(src, dest); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyDir(src, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		return copyFileMode(path, out, info.Mode())
	})
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// prepareExclusiveCodexHome builds a process-private CODEX_HOME under the
// system temp dir and writes only the named HTTP MCP servers into its
// config.toml. Never mutates ~/.codex/config.toml or the project tree.
// Auth is copied read-only from the user home when present. Callers that
// have a session id persist this dir via exclusiveCodexHomeCleanup.
func prepareExclusiveCodexHome(servers []MCPServer, sandbox codexSandboxTuning) (home string, cleanup func(), err error) {
	dest, err := os.MkdirTemp("", "claudia-mcp-codex-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dest) }
	if err := writeExclusiveCodexHome(dest, servers, sandbox); err != nil {
		cleanup()
		return "", nil, err
	}
	return dest, cleanup, nil
}

// codexSandboxTuning is the part of a Codex sandbox that thread/start
// cannot carry (🎯T598). Empty means "leave Codex's own defaults alone".
type codexSandboxTuning struct {
	WritableRoots []string
	NetworkAccess bool

	// GitRoots are the seat's own git directories (🎯T109). They are
	// written as writable roots like any other, and unlike the others
	// their absence from the echoed sandbox refuses the start.
	GitRoots []string
}

func (t codexSandboxTuning) empty() bool {
	return len(t.WritableRoots) == 0 && len(t.GitRoots) == 0 && !t.NetworkAccess
}

// codexSandboxTOML renders the [sandbox_workspace_write] stanza. Codex
// reads these from CODEX_HOME/config.toml; no RPC accepts them, so this
// file is the whole channel.
func codexSandboxTOML(t codexSandboxTuning) string {
	// Decide what there is to say BEFORE writing a header: a stanza with
	// no keys is not harmless, it overrides whatever the user's own
	// config said.
	quoted := make([]string, 0, len(t.WritableRoots)+len(t.GitRoots))
	for _, r := range slices.Concat(t.WritableRoots, t.GitRoots) {
		if r = strings.TrimSpace(r); r != "" && !slices.Contains(quoted, strconv.Quote(r)) {
			quoted = append(quoted, strconv.Quote(r))
		}
	}
	if len(quoted) == 0 && !t.NetworkAccess {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[sandbox_workspace_write]\n")
	if len(quoted) > 0 {
		b.WriteString("writable_roots = [" + strings.Join(quoted, ", ") + "]\n")
	}
	if t.NetworkAccess {
		b.WriteString("network_access = true\n")
	}
	return b.String()
}

func writeExclusiveCodexHome(dest string, servers []MCPServer, sandbox codexSandboxTuning) error {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	_ = copyFileIfExists(filepath.Join(userHome, ".codex", "auth.json"), filepath.Join(dest, "auth.json"))
	cfg := filepath.Join(dest, "config.toml")
	body := "# claudia session MCP\n" + codexSandboxTOML(sandbox)
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		return err
	}
	for _, s := range httpMCPServers(servers) {
		if _, err := upsertTOMLHTTPServer(cfg, s); err != nil {
			return fmt.Errorf("exclusive codex mcp %s: %w", s.Name, err)
		}
	}
	return nil
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

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	codexBinEnv    = "CODEX_BIN"
	codexBinName   = "codex"
	grokBinEnv     = "GROK_BIN"
	grokBinName    = "grok"
	cursorBinEnv   = "CURSOR_BIN"
	cursorBinName  = "agent"
	cursorBinAlias = "cursor-agent"
)

// Provider identifies the CLI/runtime backing a Task or Agent.
type Provider string

const (
	// ProviderClaude uses Claude Code and is the default when Provider is empty.
	ProviderClaude Provider = "claude"
	// ProviderCodex uses Codex.
	ProviderCodex Provider = "codex"
	// ProviderGrok uses the Grok Build CLI (binary name "grok").
	// Distinct from the Realtime voice client in package
	// github.com/marcelocantos/claudia/grok.
	ProviderGrok Provider = "grok"
	// ProviderBedrock uses Anthropic Claude models via AWS Bedrock
	// ConverseStream (API path; no local claude CLI). Task mode only in v1.
	ProviderBedrock Provider = "bedrock"

	// ProviderOllama is local inference through an Ollama daemon. Its
	// cost is latency rather than money, which is why it reports no cost
	// capability instead of reporting a spend of zero.
	ProviderOllama Provider = "ollama"

	// ProviderCursor uses the Cursor Agent CLI (binary name
	// "cursor-agent", often also installed as "agent"). Session mode
	// speaks ACP over `agent acp` stdio. Task mode uses
	// `agent --print --output-format stream-json`. Bare "agent" on PATH
	// is not trusted first: Grok ships its own `~/.grok/bin/agent`, which
	// rejects Cursor's --force/--trust flags.
	ProviderCursor Provider = "cursor"
)

// Capability reporting (Capability, CapabilityStatus, CapabilityError,
// the provider claim matrix) lives in capability.go.

func resolveCodexBin() (string, error) {
	return resolveCodexBinFrom(os.Getenv, exec.LookPath, os.Stat, codexBinCandidates())
}

func resolveCodexBinFrom(
	getenv func(string) string,
	lookPath func(string) (string, error),
	stat func(string) (os.FileInfo, error),
	candidates []string,
) (string, error) {
	if p := getenv(codexBinEnv); p != "" {
		if filepath.IsAbs(p) {
			if _, err := stat(p); err == nil && !isCmuxCLIShim(p) {
				return p, nil
			}
		} else if abs, err := lookPath(p); err == nil && !isCmuxCLIShim(abs) {
			return abs, nil
		}
	}
	// Prefer known install dirs over PATH: cmux injects a shim named
	// "codex" that prints "codex not found in PATH" (exit 127).
	for _, c := range candidates {
		if c == "" || isCmuxCLIShim(c) {
			continue
		}
		if _, err := stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := lookPath(codexBinName); err == nil && !isCmuxCLIShim(p) {
		return p, nil
	}
	return "", fmt.Errorf("codex executable not found in PATH or known install dirs (set %s to override)", codexBinEnv)
}

// isCmuxCLIShim reports whether p is a cmux-injected CLI wrapper, not
// the real Codex/Claude binary. Those shims fail with exit 127 when the
// wrapped tool is absent from the inner PATH.
func isCmuxCLIShim(p string) bool {
	return strings.Contains(filepath.ToSlash(p), "/cmux-cli-shims/")
}

func codexBinCandidates() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".local", "bin", codexBinName),
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
		// Post 2026-07-09 Codex app → ChatGPT desktop merger: the CLI ships
		// inside ChatGPT.app. Keep the legacy Codex.app path as a fallback.
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	}
}

func resolveGrokBin() (string, error) {
	return resolveGrokBinFrom(os.Getenv, exec.LookPath, os.Stat, grokBinCandidates())
}

func resolveGrokBinFrom(
	getenv func(string) string,
	lookPath func(string) (string, error),
	stat func(string) (os.FileInfo, error),
	candidates []string,
) (string, error) {
	if p := getenv(grokBinEnv); p != "" {
		if filepath.IsAbs(p) {
			if _, err := stat(p); err == nil {
				return p, nil
			}
		} else if abs, err := lookPath(p); err == nil {
			return abs, nil
		}
	}
	if p, err := lookPath(grokBinName); err == nil {
		return p, nil
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("grok executable not found in PATH or known install dirs (set %s to override)", grokBinEnv)
}

func grokBinCandidates() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".grok", "bin", grokBinName),
		filepath.Join(home, ".local", "bin", grokBinName),
		"/opt/homebrew/bin/grok",
		"/usr/local/bin/grok",
	}
}

func resolveCursorBin() (string, error) {
	return resolveCursorBinFrom(os.Getenv, exec.LookPath, os.Stat, cursorBinCandidates())
}

func resolveCursorBinFrom(
	getenv func(string) string,
	lookPath func(string) (string, error),
	stat func(string) (os.FileInfo, error),
	candidates []string,
) (string, error) {
	if p := getenv(cursorBinEnv); p != "" {
		if filepath.IsAbs(p) {
			if _, err := stat(p); err == nil {
				return p, nil
			}
		} else if abs, err := lookPath(p); err == nil {
			return abs, nil
		}
	}
	// Prefer the unambiguous name. Grok also installs a binary named
	// "agent" earlier on some PATHs (~/.grok/bin before ~/.local/bin).
	if p, err := lookPath(cursorBinAlias); err == nil && !isGrokAgentPath(p) {
		return p, nil
	}
	for _, c := range candidates {
		if c == "" || isGrokAgentPath(c) {
			continue
		}
		if _, err := stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := lookPath(cursorBinName); err == nil && !isGrokAgentPath(p) {
		return p, nil
	}
	return "", fmt.Errorf("cursor agent executable not found in PATH or known install dirs (set %s to override)", cursorBinEnv)
}

// isGrokAgentPath reports whether p is Grok's `agent` (not Cursor's).
// Grok installs ~/.grok/bin/agent; Cursor installs cursor-agent and a
// same-named symlink under ~/.local/bin.
func isGrokAgentPath(p string) bool {
	return strings.Contains(filepath.ToSlash(p), "/.grok/")
}

func cursorBinCandidates() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".local", "bin", cursorBinName),
		filepath.Join(home, ".local", "bin", cursorBinAlias),
		"/opt/homebrew/bin/agent",
		"/opt/homebrew/bin/cursor-agent",
		"/usr/local/bin/agent",
		"/usr/local/bin/cursor-agent",
	}
}

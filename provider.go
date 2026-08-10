// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	codexBinEnv  = "CODEX_BIN"
	codexBinName = "codex"
	grokBinEnv   = "GROK_BIN"
	grokBinName  = "grok"
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
			if _, err := stat(p); err == nil {
				return p, nil
			}
		} else if abs, err := lookPath(p); err == nil {
			return abs, nil
		}
	}
	if p, err := lookPath(codexBinName); err == nil {
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
	return "", fmt.Errorf("codex executable not found in PATH or known install dirs (set %s to override)", codexBinEnv)
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

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
)

// Provider identifies the CLI/runtime backing a Task or Agent.
type Provider string

const (
	// ProviderClaude uses Claude Code and is the default when Provider is empty.
	ProviderClaude Provider = "claude"
	// ProviderCodex uses Codex.
	ProviderCodex Provider = "codex"
)

const (
	// CapabilityUnsupported means the provider has no supported public contract
	// for the requested behavior.
	CapabilityUnsupported = "unsupported"
	// CapabilityExperimental means the provider may eventually support the
	// behavior, but claudia is deliberately failing closed until the public
	// contract is proven.
	CapabilityExperimental = "experimental"
)

// CapabilityError reports that a provider capability is unsupported or
// experimental in the current implementation.
type CapabilityError struct {
	Provider   Provider
	Capability string
	Status     string
	Reason     string
}

func (e *CapabilityError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("%s provider %s capability is %s", e.Provider, e.Capability, e.Status)
	}
	return fmt.Sprintf("%s provider %s capability is %s: %s", e.Provider, e.Capability, e.Status, e.Reason)
}

func unsupportedCapability(provider Provider, capability, reason string) *CapabilityError {
	return &CapabilityError{
		Provider:   provider,
		Capability: capability,
		Status:     CapabilityUnsupported,
		Reason:     reason,
	}
}

func experimentalCapability(provider Provider, capability, reason string) *CapabilityError {
	return &CapabilityError{
		Provider:   provider,
		Capability: capability,
		Status:     CapabilityExperimental,
		Reason:     reason,
	}
}

type providerCapabilities struct {
	Task          bool
	Session       bool
	Resume        bool
	Rewind        bool
	Cost          bool
	Permissions   bool
	TmuxAttach    bool
	TerminalBytes bool
}

func claudeProviderCapabilities() providerCapabilities {
	return providerCapabilities{
		Task:          true,
		Session:       true,
		Resume:        true,
		Rewind:        true,
		Cost:          true,
		Permissions:   true,
		TmuxAttach:    true,
		TerminalBytes: true,
	}
}

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
		"/Applications/Codex.app/Contents/Resources/codex",
	}
}

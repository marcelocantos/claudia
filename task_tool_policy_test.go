// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRestrictedTaskArgsUseNativeAllowlist(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		provider Provider
		policy   *TaskToolPolicy
		args     func(taskRunRequest) []string
		want     []string
	}{
		{"claude", ProviderClaude, &TaskToolPolicy{Builtins: []string{"Bash", "Read"}, Allow: []string{"Bash(ps -axo *)"}, Deny: []string{"Edit"}, MaxTurns: 2}, claudeTaskArgs,
			[]string{"--permission-mode", "dontAsk", "--strict-mcp-config", "--tools", "Bash,Read", "--max-turns", "2", "--allowedTools", "Bash(ps -axo *)"}},
		{"grok", ProviderGrok, &TaskToolPolicy{Builtins: []string{"run_terminal_cmd", "read_file"}, Allow: []string{"Bash(ps -axo *)"}, Deny: []string{"Bash(curl *)"}, MaxTurns: 2, HomeDir: home}, grokTaskArgs,
			[]string{"--permission-mode", "dontAsk", "--no-subagents", "--tools", "run_terminal_cmd,read_file", "--max-turns", "2", "--allow", "Bash(ps -axo *)", "--deny", "Bash(curl *)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTaskToolPolicy(tc.provider, tc.policy); err != nil {
				t.Fatal(err)
			}
			got := tc.args(taskRunRequest{Prompt: "health check", ToolPolicy: tc.policy})
			if slices.Contains(got, "bypassPermissions") || slices.Contains(got, "--dangerously-skip-permissions") {
				t.Fatalf("restricted run bypasses permissions: %v", got)
			}
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("restricted argv lacks %q: %v", want, got)
				}
			}
		})
	}
}

func TestTaskToolPolicyFailsClosed(t *testing.T) {
	policy := &TaskToolPolicy{Builtins: []string{"Read"}, MaxTurns: 1}
	for _, provider := range []Provider{ProviderCodex, ProviderCursor, ProviderBedrock, ProviderOllama} {
		var capErr *CapabilityError
		if err := validateTaskToolPolicy(provider, policy); !errors.As(err, &capErr) || capErr.Capability != CapabilityToolRestrictions {
			t.Errorf("%s accepted restricted task: %v", provider, err)
		}
	}
	if err := validateTaskToolPolicy(ProviderGrok, policy); err == nil || !strings.Contains(err.Error(), "isolated home") {
		t.Fatalf("Grok without isolated home: %v", err)
	}
	publicHome := t.TempDir()
	if err := os.Chmod(publicHome, 0o755); err != nil {
		t.Fatal(err)
	}
	policy.HomeDir = publicHome
	if err := validateTaskToolPolicy(ProviderGrok, policy); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatalf("Grok with public home: %v", err)
	}
}

func TestRestrictedGrokEnvironmentUsesPrivateHome(t *testing.T) {
	home := t.TempDir()
	auth := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("GROK_AUTH_PATH", auth)
	env, err := grokTaskPolicyEnv(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOME=" + home, "GROK_HOME=" + filepath.Join(home, ".grok"), "GROK_AUTH_PATH=" + auth} {
		if !slices.Contains(env, want) {
			t.Errorf("restricted environment lacks %q", want)
		}
	}
	info, err := os.Lstat(filepath.Join(home, ".grok"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("restricted Grok config directory: %v %v", info, err)
	}
}

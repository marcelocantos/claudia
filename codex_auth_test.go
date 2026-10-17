// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCodexAuthFixture(t *testing.T, dir string, body map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPreflightCodexAuthChatGPTSubscriptionOK(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuthFixture(t, dir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]string{
			"access_token": "tok-sub",
			"account_id":   "acct-1",
		},
	})
	pf := PreflightCodexAuth(&CodexAuthPreflightArgs{
		AuthPath: path,
		Getenv:   func(string) string { return "" },
	})
	if pf.Mode != CodexAuthModeChatGPT {
		t.Fatalf("Mode=%q, want chatgpt", pf.Mode)
	}
	if !pf.SubscriptionOK {
		t.Fatalf("SubscriptionOK=false Reason=%q Warnings=%v", pf.Reason, pf.Warnings)
	}
	if !pf.HasAccessToken {
		t.Error("HasAccessToken=false")
	}
	if pf.EnvOpenAIAPIKey {
		t.Error("EnvOpenAIAPIKey=true")
	}
}

func TestPreflightCodexAuthAPIKeyModeFails(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuthFixture(t, dir, map[string]any{
		"auth_mode":      "apikey",
		"OPENAI_API_KEY": "sk-test",
	})
	pf := PreflightCodexAuth(&CodexAuthPreflightArgs{
		AuthPath: path,
		Getenv:   func(string) string { return "" },
	})
	if pf.Mode != CodexAuthModeAPIKey {
		t.Fatalf("Mode=%q, want apikey", pf.Mode)
	}
	if pf.SubscriptionOK {
		t.Fatal("SubscriptionOK=true for API-key mode")
	}
	if len(pf.Warnings) == 0 {
		t.Fatal("expected loud API-key warning")
	}
	joined := strings.Join(pf.Warnings, " ")
	if !strings.Contains(joined, "per token") && !strings.Contains(joined, "API-key") {
		t.Errorf("warnings missing per-token/API-key signal: %v", pf.Warnings)
	}
}

func TestPreflightCodexAuthEnvAPIKeyBlocksSubscriptionOK(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuthFixture(t, dir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]string{
			"access_token": "tok-sub",
		},
	})
	pf := PreflightCodexAuth(&CodexAuthPreflightArgs{
		AuthPath: path,
		Getenv: func(k string) string {
			if k == openaiAPIKeyEnv {
				return "sk-env"
			}
			return ""
		},
	})
	if pf.Mode != CodexAuthModeChatGPT {
		t.Fatalf("Mode=%q", pf.Mode)
	}
	if pf.SubscriptionOK {
		t.Fatal("SubscriptionOK must be false when OPENAI_API_KEY env is set")
	}
	if !pf.EnvOpenAIAPIKey {
		t.Error("EnvOpenAIAPIKey=false")
	}
	if len(pf.Warnings) == 0 || !strings.Contains(strings.Join(pf.Warnings, " "), "OPENAI_API_KEY") {
		t.Errorf("expected OPENAI_API_KEY warning, got %v", pf.Warnings)
	}
}

func TestPreflightCodexAuthMissingFile(t *testing.T) {
	pf := PreflightCodexAuth(&CodexAuthPreflightArgs{
		AuthPath: filepath.Join(t.TempDir(), "missing-auth.json"),
		Getenv:   func(string) string { return "" },
	})
	if pf.SubscriptionOK {
		t.Fatal("SubscriptionOK=true for missing file")
	}
	if pf.Mode != CodexAuthModeUnknown {
		t.Fatalf("Mode=%q", pf.Mode)
	}
	if len(pf.Warnings) == 0 {
		t.Fatal("expected warning for missing auth")
	}
}

func TestPreflightCodexAuthInfersChatGPTFromTokens(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuthFixture(t, dir, map[string]any{
		"tokens": map[string]string{
			"access_token": "tok-only",
		},
	})
	pf := PreflightCodexAuth(&CodexAuthPreflightArgs{
		AuthPath: path,
		Getenv:   func(string) string { return "" },
	})
	if pf.Mode != CodexAuthModeChatGPT {
		t.Fatalf("Mode=%q, want inferred chatgpt", pf.Mode)
	}
	if !pf.SubscriptionOK {
		t.Fatalf("SubscriptionOK=false Reason=%q", pf.Reason)
	}
}

func TestEnsureCodexSubscriptionAuth(t *testing.T) {
	dir := t.TempDir()
	okPath := writeCodexAuthFixture(t, dir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens":    map[string]string{"access_token": "t"},
	})
	if _, err := ensureCodexSubscriptionAuth(&CodexAuthPreflightArgs{
		AuthPath: okPath,
		Getenv:   func(string) string { return "" },
	}); err != nil {
		t.Fatalf("ensure ok: %v", err)
	}

	badPath := writeCodexAuthFixture(t, dir, map[string]any{
		"auth_mode": "apikey",
	})
	if _, err := ensureCodexSubscriptionAuth(&CodexAuthPreflightArgs{
		AuthPath: badPath,
		Getenv:   func(string) string { return "" },
	}); err == nil {
		t.Fatal("ensure apikey: expected error")
	}
}

func TestCodexBinCandidatesResolveOnThisHost(t *testing.T) {
	// A present ChatGPT or Codex app bundle used to win resolution.
	// The product resolver now refuses before it stats those paths.
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BIN", fake)
	t.Setenv("PATH", dir)
	if _, err := resolveCodexBin(); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("resolveCodexBin = %v, want legacy refusal", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("resolveCodexBin started codex: %v", statErr)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
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
	// Integration-ish: when ChatGPT.app is installed (owner fleet), resolveCodexBin
	// must find it without CODEX_BIN. Skip if neither app bundle exists.
	chatgpt := "/Applications/ChatGPT.app/Contents/Resources/codex"
	legacy := "/Applications/Codex.app/Contents/Resources/codex"
	if _, err := os.Stat(chatgpt); err != nil {
		if _, err2 := os.Stat(legacy); err2 != nil {
			t.Skip("neither ChatGPT.app nor Codex.app codex binary present")
		}
	}
	t.Setenv("CODEX_BIN", "")
	// Clear PATH codex if any so we exercise candidates.
	t.Setenv("PATH", "/usr/bin:/bin")
	got, err := resolveCodexBin()
	if err != nil {
		t.Fatalf("resolveCodexBin: %v", err)
	}
	if got != chatgpt && got != legacy {
		// Still OK if LookPath found something else first; only fail when
		// candidates should have won (PATH has no codex).
		if _, err := os.Stat(got); err != nil {
			t.Fatalf("resolved path missing: %s", got)
		}
	}
}

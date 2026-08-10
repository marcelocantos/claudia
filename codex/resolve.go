// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	binEnv      = "CODEX_BIN"
	binName     = "codex"
	authPathEnv = "CLAUDIA_CODEX_AUTH_PATH"
	openaiKey   = "OPENAI_API_KEY"
)

// ResolveArgs customises binary resolution and auth preflight (tests).
// Nil is valid (defaults). Prefer a struct over functional options.
type ResolveArgs struct {
	// BinPath forces a codex binary path (skips resolution).
	BinPath string
	// AuthPath overrides ~/.codex/auth.json (or CLAUDIA_CODEX_AUTH_PATH).
	AuthPath string
	// Getenv overrides os.Getenv. Nil uses os.Getenv.
	Getenv func(string) string
	// LookPath overrides exec.LookPath. Nil uses exec.LookPath.
	LookPath func(string) (string, error)
	// Stat overrides os.Stat. Nil uses os.Stat.
	Stat func(string) (os.FileInfo, error)
	// SkipAuthPreflight skips subscription auth (tests only).
	SkipAuthPreflight bool
}

func getenv(args *ResolveArgs) func(string) string {
	if args != nil && args.Getenv != nil {
		return args.Getenv
	}
	return os.Getenv
}

func resolveBin(args *ResolveArgs) (string, error) {
	if args != nil && strings.TrimSpace(args.BinPath) != "" {
		return args.BinPath, nil
	}
	ge := getenv(args)
	lookPath := exec.LookPath
	stat := os.Stat
	if args != nil {
		if args.LookPath != nil {
			lookPath = args.LookPath
		}
		if args.Stat != nil {
			stat = args.Stat
		}
	}

	if p := strings.TrimSpace(ge(binEnv)); p != "" {
		if filepath.IsAbs(p) {
			if _, err := stat(p); err == nil {
				return p, nil
			}
		} else if abs, err := lookPath(p); err == nil {
			return abs, nil
		}
	}
	if p, err := lookPath(binName); err == nil {
		return p, nil
	}
	for _, c := range binCandidates() {
		if c == "" {
			continue
		}
		if _, err := stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("codex executable not found in PATH or known install dirs (set %s to override)", binEnv)
}

func binCandidates() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".local", "bin", binName),
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	}
}

// authFile is the subset of ~/.codex/auth.json needed for subscription preflight.
type authFile struct {
	AuthMode     string `json:"auth_mode"`
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	Tokens       *struct {
		AccessToken string `json:"access_token"`
	} `json:"tokens"`
}

// ensureSubscriptionAuth fails closed unless ChatGPT OAuth is active and
// OPENAI_API_KEY is not set in the environment (per-token fall-through).
func ensureSubscriptionAuth(args *ResolveArgs) error {
	if args != nil && args.SkipAuthPreflight {
		return nil
	}
	ge := getenv(args)

	if strings.TrimSpace(ge(openaiKey)) != "" {
		return &AuthError{Message: "OPENAI_API_KEY is set — unset it for ChatGPT subscription (no per-token) path"}
	}

	path := ""
	if args != nil {
		path = strings.TrimSpace(args.AuthPath)
	}
	if path == "" {
		path = strings.TrimSpace(ge(authPathEnv))
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return &AuthError{Message: fmt.Sprintf("home dir: %v", err)}
		}
		path = filepath.Join(home, ".codex", "auth.json")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return &AuthError{Message: fmt.Sprintf("read %s: %v (run codex login)", path, err)}
	}
	var auth authFile
	if err := json.Unmarshal(raw, &auth); err != nil {
		return &AuthError{Message: fmt.Sprintf("parse %s: %v", path, err)}
	}

	hasToken := auth.Tokens != nil && strings.TrimSpace(auth.Tokens.AccessToken) != ""
	mode := strings.ToLower(strings.TrimSpace(auth.AuthMode))
	chatgpt := mode == "chatgpt" || mode == "chat_gpt" || mode == "oauth" || mode == "subscription" ||
		(mode == "" && hasToken)

	if mode == "apikey" || mode == "api_key" || mode == "api-key" || mode == "openai" {
		return &AuthError{Message: "auth_mode is apikey (per-token billing); run codex login for ChatGPT subscription"}
	}
	if !chatgpt || !hasToken {
		return &AuthError{Message: "ChatGPT subscription OAuth not confirmed (auth_mode=chatgpt + tokens.access_token required)"}
	}
	return nil
}

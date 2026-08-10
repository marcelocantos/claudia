// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// codexAuthPathEnv overrides ~/.codex/auth.json for preflight and tests.
	codexAuthPathEnv = "CLAUDIA_CODEX_AUTH_PATH"
	// openaiAPIKeyEnv is the OpenAI per-token billing credential. When set,
	// Codex may fall through to API-key auth even if ChatGPT OAuth is present.
	openaiAPIKeyEnv = "OPENAI_API_KEY"
)

// CodexAuthMode is how Codex authenticates for model calls.
type CodexAuthMode string

const (
	// CodexAuthModeChatGPT is ChatGPT-subscription OAuth (zero per-token).
	CodexAuthModeChatGPT CodexAuthMode = "chatgpt"
	// CodexAuthModeAPIKey is OpenAI API-key / per-token billing.
	CodexAuthModeAPIKey CodexAuthMode = "apikey"
	// CodexAuthModeUnknown means auth.json is missing, empty, or unrecognized.
	CodexAuthModeUnknown CodexAuthMode = "unknown"
)

// CodexAuthPreflightArgs configures [PreflightCodexAuth].
// Nil is valid (defaults). Prefer a struct over functional options.
type CodexAuthPreflightArgs struct {
	// AuthPath overrides the path to Codex auth.json.
	// Empty means CLAUDIA_CODEX_AUTH_PATH, else ~/.codex/auth.json.
	AuthPath string
	// Getenv overrides os.Getenv (tests). Nil uses os.Getenv.
	Getenv func(string) string
}

// CodexAuthPreflight is the result of inspecting Codex auth for the
// no-per-token (ChatGPT subscription) invariant.
type CodexAuthPreflight struct {
	// Mode is the active auth mode (chatgpt | apikey | unknown).
	Mode CodexAuthMode
	// AuthPath is the auth.json path that was read (or attempted).
	AuthPath string
	// HasAccessToken is true when tokens.access_token is non-empty.
	HasAccessToken bool
	// HasAPIKeyInFile is true when auth.json carries a non-empty OPENAI_API_KEY.
	HasAPIKeyInFile bool
	// EnvOpenAIAPIKey is true when OPENAI_API_KEY is set in the process env.
	EnvOpenAIAPIKey bool
	// SubscriptionOK is true when ChatGPT-subscription OAuth is the active
	// path (auth_mode=chatgpt with an access token) and no env API key is
	// present to force per-token billing.
	SubscriptionOK bool
	// Warnings are loud human-readable notices (API-key fall-through risk).
	Warnings []string
	// Reason explains why SubscriptionOK is false (empty when OK).
	Reason string
}

type codexAuthFileFull struct {
	AuthMode     string `json:"auth_mode"`
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	Tokens       *struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// PreflightCodexAuth reports the active Codex auth mode and asserts the
// ChatGPT-subscription (OAuth) no-per-token path when possible.
//
// It does not contact the network. Callers that must not bill per token
// should refuse to spawn when SubscriptionOK is false.
func PreflightCodexAuth(args *CodexAuthPreflightArgs) CodexAuthPreflight {
	if args == nil {
		args = &CodexAuthPreflightArgs{}
	}
	getenv := args.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	path := strings.TrimSpace(args.AuthPath)
	if path == "" {
		path = strings.TrimSpace(getenv(codexAuthPathEnv))
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return CodexAuthPreflight{
				Mode:     CodexAuthModeUnknown,
				Reason:   fmt.Sprintf("home dir: %v", err),
				Warnings: []string{"Codex auth preflight: cannot resolve home directory"},
			}
		}
		path = filepath.Join(home, ".codex", "auth.json")
	}

	result := CodexAuthPreflight{
		Mode:            CodexAuthModeUnknown,
		AuthPath:        path,
		EnvOpenAIAPIKey: strings.TrimSpace(getenv(openaiAPIKeyEnv)) != "",
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		result.Reason = fmt.Sprintf("read %s: %v", path, err)
		result.Warnings = append(result.Warnings,
			"Codex auth preflight: no auth.json — ChatGPT subscription login required (codex login); API-key path bills per token")
		return result
	}

	var auth codexAuthFileFull
	if err := json.Unmarshal(raw, &auth); err != nil {
		result.Reason = fmt.Sprintf("parse %s: %v", path, err)
		result.Warnings = append(result.Warnings,
			"Codex auth preflight: auth.json unreadable — refusing to assume subscription auth")
		return result
	}

	if auth.Tokens != nil && strings.TrimSpace(auth.Tokens.AccessToken) != "" {
		result.HasAccessToken = true
	}
	if strings.TrimSpace(auth.OpenAIAPIKey) != "" {
		result.HasAPIKeyInFile = true
	}

	mode := strings.ToLower(strings.TrimSpace(auth.AuthMode))
	switch mode {
	case "chatgpt", "chat_gpt", "oauth", "subscription":
		result.Mode = CodexAuthModeChatGPT
	case "apikey", "api_key", "api-key", "openai":
		result.Mode = CodexAuthModeAPIKey
	case "":
		// Infer: access token without auth_mode still implies ChatGPT OAuth.
		if result.HasAccessToken {
			result.Mode = CodexAuthModeChatGPT
		} else if result.HasAPIKeyInFile {
			result.Mode = CodexAuthModeAPIKey
		}
	default:
		result.Mode = CodexAuthModeUnknown
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Codex auth preflight: unrecognized auth_mode %q", auth.AuthMode))
	}

	if result.EnvOpenAIAPIKey {
		result.Warnings = append(result.Warnings,
			"Codex auth preflight: OPENAI_API_KEY is set in the environment — "+
				"Codex may bill per token instead of the ChatGPT subscription; unset it for the no-per-token path")
	}
	if result.HasAPIKeyInFile && result.Mode != CodexAuthModeChatGPT {
		result.Warnings = append(result.Warnings,
			"Codex auth preflight: auth.json carries OPENAI_API_KEY without ChatGPT OAuth — per-token billing path")
	}
	if result.Mode == CodexAuthModeAPIKey {
		result.Warnings = append(result.Warnings,
			"Codex auth preflight: active auth_mode is API-key — model calls bill per token; "+
				"run `codex login` (ChatGPT) for subscription auth")
	}

	switch {
	case result.Mode == CodexAuthModeChatGPT && result.HasAccessToken && !result.EnvOpenAIAPIKey:
		result.SubscriptionOK = true
	case result.Mode == CodexAuthModeChatGPT && result.HasAccessToken && result.EnvOpenAIAPIKey:
		result.Reason = "ChatGPT OAuth present but OPENAI_API_KEY env may force per-token billing"
	case result.Mode == CodexAuthModeChatGPT && !result.HasAccessToken:
		result.Reason = "auth_mode is chatgpt but tokens.access_token is empty — re-login required"
	case result.Mode == CodexAuthModeAPIKey:
		result.Reason = "auth_mode is apikey (per-token billing)"
	default:
		if result.Reason == "" {
			result.Reason = "ChatGPT subscription OAuth not confirmed"
		}
	}
	return result
}

// ensureCodexSubscriptionAuth runs [PreflightCodexAuth] and returns an error
// when the ChatGPT-subscription path is not active. Warnings are returned for
// the caller to log even on success (e.g. residual API-key signals).
func ensureCodexSubscriptionAuth(args *CodexAuthPreflightArgs) (CodexAuthPreflight, error) {
	pf := PreflightCodexAuth(args)
	if !pf.SubscriptionOK {
		msg := "codex subscription auth preflight failed"
		if pf.Reason != "" {
			msg += ": " + pf.Reason
		}
		if len(pf.Warnings) > 0 {
			msg += " [" + strings.Join(pf.Warnings, "; ") + "]"
		}
		return pf, fmt.Errorf("%s", msg)
	}
	return pf, nil
}

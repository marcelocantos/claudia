// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"fmt"
	"strings"
)

// AuthError is a Codex authentication / subscription-auth failure.
// Returned from [Task.Run] when preflight fails, and wrapped in
// [Event.Error] when JSONL reports auth/login failures.
type AuthError struct {
	Message string
}

func (e *AuthError) Error() string {
	if e == nil {
		return "codex: auth error"
	}
	if e.Message == "" {
		return "codex: auth error"
	}
	return "codex: auth error: " + e.Message
}

// RateLimitError is a throttle / rate-limit / usage-cap failure from Codex.
type RateLimitError struct {
	Message string
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "codex: rate limit"
	}
	if e.Message == "" {
		return "codex: rate limit"
	}
	return "codex: rate limit: " + e.Message
}

// ExitError is a non-zero process exit from `codex exec` when no structured
// JSONL error was already emitted.
type ExitError struct {
	// ExitCode is the process exit code (0 when unknown but failed).
	ExitCode int
	// Message is optional detail (stderr snippet or Wait error text).
	Message string
}

func (e *ExitError) Error() string {
	if e == nil {
		return "codex: non-zero exit"
	}
	if e.Message != "" {
		return fmt.Sprintf("codex: exit %d: %s", e.ExitCode, e.Message)
	}
	return fmt.Sprintf("codex: exit %d", e.ExitCode)
}

// RunError is a structured Codex turn/run failure that is neither auth,
// rate-limit, nor process-exit classified.
type RunError struct {
	Message string
}

func (e *RunError) Error() string {
	if e == nil {
		return "codex: run error"
	}
	if e.Message == "" {
		return "codex: run error"
	}
	return "codex: " + e.Message
}

// ClassifyFailure maps a free-text Codex failure message onto a typed error.
// Prefer the concrete *AuthError / *RateLimitError when the message matches
// known patterns; otherwise returns *RunError.
func ClassifyFailure(msg string) error {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return &RunError{Message: "unknown failure"}
	}
	lower := strings.ToLower(msg)
	switch {
	case isRateLimitMessage(lower):
		return &RateLimitError{Message: msg}
	case isAuthMessage(lower):
		return &AuthError{Message: msg}
	default:
		return &RunError{Message: msg}
	}
}

func isRateLimitMessage(lower string) bool {
	needles := []string{
		"rate limit",
		"rate_limit",
		"ratelimit",
		"too many requests",
		"usage limit",
		"quota exceeded",
		"throttl",
		"429",
	}
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func isAuthMessage(lower string) bool {
	needles := []string{
		"unauthorized",
		"unauthenticated",
		"not authenticated",
		"not logged",
		"please log in",
		"please login",
		"run codex login",
		"codex login",
		"auth_mode",
		"authentication",
		"invalid token",
		"access token",
		"api key",
		"api_key",
		"openai_api_key",
		"subscription auth",
		"per-token billing",
	}
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	// Standalone "auth" / "login" only when they appear as clear tokens.
	if strings.Contains(lower, " auth") || strings.HasPrefix(lower, "auth ") ||
		strings.Contains(lower, "login required") || strings.Contains(lower, "login:") {
		return true
	}
	return false
}

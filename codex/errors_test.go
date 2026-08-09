// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"errors"
	"testing"
)

func TestClassifyFailureRateLimit(t *testing.T) {
	msgs := []string{
		"rate limit exceeded: try again later (429)",
		"Too Many Requests",
		"usage limit reached",
		"quota exceeded for plan",
		"request was throttled",
	}
	for _, msg := range msgs {
		err := ClassifyFailure(msg)
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Errorf("ClassifyFailure(%q) = %T %v, want *RateLimitError", msg, err, err)
		}
	}
}

func TestClassifyFailureAuth(t *testing.T) {
	msgs := []string{
		"unauthorized: please run codex login",
		"not authenticated",
		"invalid token",
		"OPENAI_API_KEY is set — per-token billing",
		"subscription auth preflight failed",
	}
	for _, msg := range msgs {
		err := ClassifyFailure(msg)
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Errorf("ClassifyFailure(%q) = %T %v, want *AuthError", msg, err, err)
		}
	}
}

func TestClassifyFailureGeneric(t *testing.T) {
	err := ClassifyFailure("model failed")
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("got %T %v, want *RunError", err, err)
	}
	if re.Message != "model failed" {
		t.Errorf("Message = %q", re.Message)
	}
}

func TestExitErrorMessage(t *testing.T) {
	err := &ExitError{ExitCode: 2, Message: "boom"}
	if got := err.Error(); got != "codex: exit 2: boom" {
		t.Errorf("Error() = %q", got)
	}
}

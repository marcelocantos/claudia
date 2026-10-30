// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marcelocantos/claudia/codex"
)

func TestProviderCauseSentinelsKeepTheirText(t *testing.T) {
	cases := []struct {
		err   error
		text  string
		cause ProviderCause
	}{
		{ErrTurnInFlight, "prompt already in flight", CauseTurnInFlight},
		{ErrProcessNotRunning, "claude process not running", CauseProcessNotRunning},
		{ErrSteerUnsupported, "steer unsupported", CauseSteerUnsupported},
		{ErrTurnIdle, "no turn in flight to steer", CauseTurnIdle},
		{caused(CauseTurnInFlight, "codex app-server: turn already in flight", ErrTurnInFlight), "codex app-server: turn already in flight", CauseTurnInFlight},
		{fmt.Errorf("grok acp: %w", ErrTurnInFlight), "grok acp: prompt already in flight", CauseTurnInFlight},
		{fmt.Errorf("cursor acp: %w", ErrTurnInFlight), "cursor acp: prompt already in flight", CauseTurnInFlight},
		{fmt.Errorf("%w: claude provider: provider has no steer mechanism", ErrSteerUnsupported), "steer unsupported: claude provider: provider has no steer mechanism", CauseSteerUnsupported},
	}
	for _, tc := range cases {
		if tc.err.Error() != tc.text {
			t.Errorf("Error() = %q, want %q", tc.err.Error(), tc.text)
		}
		var c interface{ ProviderCause() string }
		if !errors.As(tc.err, &c) || c.ProviderCause() != string(tc.cause) {
			t.Errorf("cause of %q = %v, want %s", tc.err, c, tc.cause)
		}
	}
	if !errors.Is(fmt.Errorf("grok acp: %w", ErrTurnInFlight), ErrTurnInFlight) {
		t.Fatal("wrapped turn does not unwrap to ErrTurnInFlight")
	}
	if !errors.Is(caused(CauseTurnInFlight, "codex app-server: turn already in flight", ErrTurnInFlight), ErrTurnInFlight) {
		t.Fatal("codex turn does not unwrap to ErrTurnInFlight")
	}
	plain := errors.New("401 unauthorized")
	var c interface{ ProviderCause() string }
	if errors.As(plain, &c) {
		t.Fatal("prose 401 carried a cause")
	}
}

func TestCodexAuthAndRateLimitCarryCauses(t *testing.T) {
	auth := &codex.AuthError{Message: "401 unauthorized"}
	if got := auth.ProviderCause(); got != string(CauseAuth) {
		t.Fatalf("auth cause %q", got)
	}
	if auth.Error() != "codex: auth error: 401 unauthorized" {
		t.Fatalf("auth text changed: %q", auth.Error())
	}
	limit := &codex.RateLimitError{Message: "slow down"}
	if got := limit.ProviderCause(); got != string(CauseRateLimit) {
		t.Fatalf("rate cause %q", got)
	}
	if limit.Error() != "codex: rate limit: slow down" {
		t.Fatalf("rate text changed: %q", limit.Error())
	}
}

func TestCauseFromProviderCodeIgnoresSentences(t *testing.T) {
	if got := CauseFromProviderCode("rate_limit"); got != CauseRateLimit {
		t.Fatalf("rate_limit = %q", got)
	}
	if got := CauseFromProviderCode("rate_limit_error"); got != CauseRateLimit {
		t.Fatalf("rate_limit_error = %q", got)
	}
	if got := CauseFromProviderCode("billing_error"); got != CauseSpendWall {
		t.Fatalf("billing_error = %q", got)
	}
	if CauseSpendWall != "spend_wall" {
		t.Fatalf("spend wall spelling %q", CauseSpendWall)
	}
	if got := CauseFromProviderCode("authentication_failed"); got != CauseAuth {
		t.Fatalf("authentication_failed = %q", got)
	}
	if got := CauseFromProviderCode("authentication_error"); got != CauseAuth {
		t.Fatalf("authentication_error = %q", got)
	}
	if got := CauseFromProviderCode("model_not_found"); got != CauseClientBug {
		t.Fatalf("model_not_found = %q", got)
	}
	if got := CauseFromProviderCode("overloaded_error"); got != CauseBackendUnavailable {
		t.Fatalf("overloaded_error = %q", got)
	}
	if got := CauseFromProviderCode("internal_error"); got != CauseBackendUnavailable {
		t.Fatalf("internal_error = %q", got)
	}
	if got := CauseFromProviderCode("401 Unauthorized"); got != "" {
		t.Fatalf("sentence mapped to %q", got)
	}
	if got := CauseFromProviderCode("you are rate limited"); got != "" {
		t.Fatalf("sentence mapped to %q", got)
	}
}

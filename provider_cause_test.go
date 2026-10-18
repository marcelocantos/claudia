// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"testing"
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

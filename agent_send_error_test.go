// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"testing"
)

// 🎯T28 acceptance 4: nothing between a provider send verdict and
// Agent.Send's return may swallow or downgrade it.

// TestSendPropagatesProviderSendFailure closes the generic swallow:
// Agent.Send returns the provider operation's error verbatim rather
// than reporting the keystrokes-left-the-process success that made a
// lost submit invisible above agent.go's Send.
func TestSendPropagatesProviderSendFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("turn not submitted: composer state=typed_unsubmitted after 8 Enter presses")
	ready := make(chan struct{})
	close(ready)
	a := &Agent{
		provider: ProviderClaude,
		alive:    true,
		ready:    ready,
		ops: agentOps{
			send: func(*Agent, string) error { return wantErr },
		},
	}
	err := a.Send("a brief that never became a turn")
	if err == nil {
		t.Fatal("Agent.Send returned nil while the provider reported the turn was never submitted")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Agent.Send error = %v, want it to carry %v", err, wantErr)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
)

// 🎯T601. Every provider but the tmux Claude session implemented
// promptInFlight, so Agent.PromptInFlight() hit the nil guard and returned
// a confident false for Claude — not "unknown", but "no turn is running"
// while one visibly was. A caller deciding whether an agent is wedged got
// the wrong answer exactly when a turn was long, which is when it matters.
func TestClaudeSessionAnswersPromptInFlight(t *testing.T) {
	if claudeAgentOps().promptInFlight == nil {
		t.Fatal("claude tmux session still cannot answer whether a turn is running")
	}
}

// Every Session provider must answer. A nil here is not a small gap: the
// call site cannot tell "no" from "cannot say", and the API offers no way
// to express the difference.
func TestEverySessionBackendAnswersPromptInFlight(t *testing.T) {
	if claudeAgentOps().promptInFlight == nil {
		t.Error("claude: promptInFlight unset — PromptInFlight() will answer false")
	}
}

// A window that is not there is not a turn in flight, and must not panic
// or shell out.
func TestPromptInFlightIsFalseWithoutAWindow(t *testing.T) {
	fn := claudeAgentOps().promptInFlight
	if fn(nil) {
		t.Fatal("a nil agent reported a turn in flight")
	}
	if fn(&Agent{}) {
		t.Fatal("an agent with no tmux window reported a turn in flight")
	}
}

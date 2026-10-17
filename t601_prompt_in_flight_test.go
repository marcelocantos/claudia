// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"strings"
	"testing"
)

// The sidecar seat answers whether a turn is running from its control
// inflight flag. A nil answer is a confident false, which is the bug
// 🎯T601 closed for the old tmux session.
func TestClaudeSessionAnswersPromptInFlight(t *testing.T) {
	src, err := os.ReadFile("omp_agent.go")
	if err != nil {
		t.Fatal(err)
	}
	const needle = "promptInFlight: func(*Agent) bool { return ctrl.inflight.Load() }"
	if !strings.Contains(string(src), needle) {
		t.Fatal("sidecar seat does not answer promptInFlight from its inflight flag")
	}
}

func TestEverySessionBackendAnswersPromptInFlight(t *testing.T) {
	src, err := os.ReadFile("omp_agent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "promptInFlight:") {
		t.Error("sidecar ops leave promptInFlight unset — PromptInFlight() will answer false")
	}
}

func TestPromptInFlightIsFalseWithoutAWindow(t *testing.T) {
	if (&Agent{}).PromptInFlight() {
		t.Fatal("an agent with no operation reported a turn in flight")
	}
	var a *Agent
	if a.PromptInFlight() {
		t.Fatal("a nil agent reported a turn in flight")
	}
}

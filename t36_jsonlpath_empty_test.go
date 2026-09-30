// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"path/filepath"
	"testing"
)

// TestStartJSONLPathEmptyForBackendsWithNoTranscript is the 🎯T36 oracle: a
// backend that leaves agentStart.JSONLPath empty AND declines to tail
// (TailJSONL: false) is making a deliberate claim — "no ~/.claude/projects
// transcript exists for this session" — not "no override was given". Start
// must honour that claim rather than keeping the precomputed
// SessionJSONLPath default it computed before dispatching to the backend.
// Claude-shaped agents (TailJSONL: true, the fake-claude default) must keep
// their transcript path unaffected.
func TestStartJSONLPathEmptyForBackendsWithNoTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	codexAgent, err := startWithBackend(Config{
		Provider:  ProviderCodex,
		WorkDir:   t.TempDir(),
		SessionID: "thr-t36-codex",
	}, &fakeAgentBackend{name: "fake-codex"})
	if err != nil {
		t.Fatalf("codex start: %v", err)
	}
	t.Cleanup(codexAgent.Stop)
	if got := codexAgent.JSONLPath(); got != "" {
		t.Fatalf("codex JSONLPath() = %q, want empty (backend declined transcript+tail)", got)
	}

	grokAgent, err := startWithBackend(Config{
		Provider:  ProviderGrok,
		WorkDir:   t.TempDir(),
		SessionID: "sid-t36-grok",
	}, &fakeAgentBackend{name: "fake-grok"})
	if err != nil {
		t.Fatalf("grok start: %v", err)
	}
	t.Cleanup(grokAgent.Stop)
	if got := grokAgent.JSONLPath(); got != "" {
		t.Fatalf("grok JSONLPath() = %q, want empty (backend declined transcript+tail)", got)
	}

	claudeWorkDir := t.TempDir()
	claudeAgent, err := startWithBackend(Config{
		Provider:  ProviderClaude,
		WorkDir:   claudeWorkDir,
		SessionID: "sid-t36-claude",
	}, &fakeAgentBackend{name: "fake-claude"})
	if err != nil {
		t.Fatalf("claude start: %v", err)
	}
	t.Cleanup(claudeAgent.Stop)
	if got := claudeAgent.JSONLPath(); got == "" || filepath.Base(got) != "sid-t36-claude.jsonl" {
		t.Fatalf("claude JSONLPath() = %q, want the Claude-shaped transcript path unaffected", got)
	}
}

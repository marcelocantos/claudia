// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🎯T11.2 hermetic oracles for advertised ProviderClaude behaviours.
// Live residual (real TUI readiness, Anthropic auth) stays under CLAUDIA_LIVE=1.

func TestClaudeProviderCapabilitiesClaimed(t *testing.T) {
	caps := claudeProviderCapabilities()
	if caps.Task || !caps.Session || !caps.Resume || !caps.Rewind ||
		!caps.Cost || !caps.Permissions || !caps.TmuxAttach || !caps.TerminalBytes {
		t.Fatalf("claudeProviderCapabilities = %+v, want full matrix true", caps)
	}
	// Production backends must agree with the claim.

	if got := (claudeAgentBackend{}).Capabilities(); got != caps {
		t.Errorf("claudeAgentBackend capabilities = %+v, want %+v", got, caps)
	}
}

// Readiness for Cancel/Stop now lives in hermeticTaskWatch
// (hermetic_wait_test.go). The deadline-polling waitHermeticTaskReady it
// replaces is what 🎯T31 removed: it decided the verdict by wall clock, so
// fleet load — not the product — chose the colour.

func TestHermeticClaudeRequireResumeFailsClosed(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	workDir := t.TempDir()
	backend := &fakeAgentBackend{name: "fake-claude"}
	_, err := startWithBackend(Config{
		WorkDir:       workDir,
		SessionID:     "materialized-missing-jsonl",
		RequireResume: true,
		TermLogPath:   "-",
	}, backend)
	if err == nil {
		t.Fatal("Start with RequireResume and missing JSONL returned nil")
	}
	if !strings.Contains(err.Error(), "refusing to mint") {
		t.Errorf("error = %v, want refuse-to-mint wording", err)
	}
	// Backend must not have been asked to spawn.
	backend.mu.Lock()
	n := len(backend.requests)
	backend.mu.Unlock()
	if n != 0 {
		t.Errorf("backend StartAgent calls = %d, want 0", n)
	}
}

func TestHermeticClaudeSessionResumeDecision(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	workDir := t.TempDir()
	// Resolve the same way Start does so SessionExists path matches.
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	sessionID := "resume-decision-session"
	jsonlPath := SessionJSONLPath(sessionID, workDir)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	backend := &fakeAgentBackend{name: "fake-claude"}
	agent, err := startWithBackend(Config{
		WorkDir:     workDir,
		SessionID:   sessionID,
		TermLogPath: "-",
	}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()

	req := backend.request(t)
	if !req.Resuming {
		t.Error("Resuming = false, want true when JSONL exists")
	}
	if req.SessionID != sessionID {
		t.Errorf("SessionID = %q", req.SessionID)
	}
}

func TestHermeticClaudeRequireResumeAllowsExistingJSONL(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	workDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	sessionID := "require-resume-ok"
	jsonlPath := SessionJSONLPath(sessionID, workDir)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	backend := &fakeAgentBackend{name: "fake-claude"}
	agent, err := startWithBackend(Config{
		WorkDir:       workDir,
		SessionID:     sessionID,
		RequireResume: true,
		TermLogPath:   "-",
	}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()
	if !backend.request(t).Resuming {
		t.Error("Resuming = false under RequireResume with present JSONL")
	}
}

func TestHermeticClaudeSessionJSONLTail(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	workDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	sessionID := "jsonl-tail-session"
	jsonlPath := SessionJSONLPath(sessionID, workDir)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	backend := &fakeAgentBackend{name: "fake-claude", tailJSONL: true}
	agent, err := startWithBackend(Config{
		WorkDir:     workDir,
		SessionID:   sessionID,
		TermLogPath: "-",
	}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()

	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	done := make(chan struct {
		text string
		err  error
	}, 1)
	// t.Context(): the 3s budget was a verdict input, not cleanup (🎯T31).
	ctx := t.Context()
	go func() {
		text, err := agent.WaitForResponse(ctx)
		done <- struct {
			text string
			err  error
		}{text, err}
	}()

	// The event is only observed if the subscriber is already installed, so
	// wait for the subscriber itself rather than sleeping 50ms and hoping —
	// under load that sleep expires before the goroutine is scheduled (🎯T31).
	waitForEventSubscribers(t, agent, 1)
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"tail hello"}],"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":2}}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(line), 0o644); err != nil {
		t.Fatalf("WriteFile JSONL: %v", err)
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("WaitForResponse: %v", r.err)
	}
	if r.text != "tail hello" {
		t.Errorf("text = %q, want tail hello", r.text)
	}

	u := agent.Usage()
	if u.InputTokens != 1 || u.OutputTokens != 2 {
		t.Errorf("Usage = %+v, want in=1 out=2", u)
	}
}

func TestHermeticClaudeSessionFreshStartNotResuming(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	backend := &fakeAgentBackend{name: "fake-claude"}
	agent, err := startWithBackend(Config{
		WorkDir:     t.TempDir(),
		SessionID:   "brand-new-session",
		TermLogPath: "-",
	}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()
	if backend.request(t).Resuming {
		t.Error("Resuming = true for missing JSONL, want false (--session-id path)")
	}
}

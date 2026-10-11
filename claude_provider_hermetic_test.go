// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 🎯T11.2 hermetic oracles for advertised ProviderClaude behaviours.
// Live residual (real TUI readiness, Anthropic auth) stays under CLAUDIA_LIVE=1.

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

// writeArgCaptureCLI is like writeFakeCLI but records argv to argsPath first.
func writeArgCaptureCLI(t *testing.T, fixturePath, argsPath string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fake CLI")
	}
	absFixture, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude-args")
	// Quote paths for sh; args go on one line space-separated.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > \"" + argsPath + "\"\n" +
		"cat \"" + absFixture + "\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return bin
}

// writeSlowFakeCLI emits the fixture init line then hangs until SIGINT/SIGTERM.
// Cancel can be observed without leaving orphans.
//
// Implementation is a single-process perl helper — not a shell script that
// forks `sleep`. Under bash/sh, a SIGINT trap is deferred until a foreground
// child completes, so Process.Signal(SIGINT) leaves the process alive until
// sleep ends; grandchildren also hold StdoutPipe open so Scan never sees EOF
// (bimodal hang under -race / loaded CI — 🎯T17). Perl handles SIGINT in-process
// and exits, releasing the pipe deterministically.
func writeSlowFakeCLI(t *testing.T, fixturePath string, sleepFor time.Duration) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal fake CLI")
	}
	absFixture, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude-slow")
	sec := int(sleepFor / time.Second)
	if sec < 1 {
		sec = 1
	}
	// Emit first fixture line (init), flush, hang until signal.
	// Quote paths for the generated perl source.
	script := "#!/usr/bin/env perl\n" +
		"use strict;\n" +
		"use warnings;\n" +
		"$SIG{INT} = $SIG{TERM} = sub { exit 0 };\n" +
		"my $fixture = \"" + absFixture + "\";\n" +
		"if (open my $fh, '<', $fixture) {\n" +
		"  my $line = <$fh>;\n" +
		"  close $fh;\n" +
		"  print $line if defined $line;\n" +
		"} \n" +
		"STDOUT->flush;\n" +
		"select(undef, undef, undef, " + strconv.Itoa(sec) + ");\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return bin
}

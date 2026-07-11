// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFakeGrokACP(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Grok ACP uses a POSIX shell wrapper")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake ACP server")
	}
	py, err := filepath.Abs("testdata/grok/acp/fake_acp.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "grok")
	script := "#!/bin/sh\n" +
		"# Ignore agent/stdio flags; speak ACP on stdio.\n" +
		"exec python3 \"" + py + "\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestHermeticGrokSessionStartSendWait(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)

	workDir := t.TempDir()
	agent, err := Start(Config{
		Provider:    ProviderGrok,
		WorkDir:     workDir,
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if agent.SessionID() == "" {
		t.Fatal("empty session id")
	}
	if !agent.Alive() {
		t.Fatal("agent not alive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Subscribe before Send (same pattern as Run).
	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		text, err := agent.WaitForResponse(ctx)
		ch <- outcome{text, err}
	}()
	runtime.Gosched()

	if err := agent.Send("Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for response")
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("WaitForResponse: %v", out.err)
		}
		if !strings.Contains(out.text, "pong") {
			t.Fatalf("response %q, want pong", out.text)
		}
	}

	u := agent.Usage()
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		// Usage is optional on terminal event; fake sends it.
		t.Logf("usage zero (acceptable if not accumulated): %+v", u)
	}
}

func TestHermeticGrokSessionRunHelper(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	text, err := Run(ctx, "Reply with exactly: pong", Config{
		Provider:    ProviderGrok,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("Run text %q, want pong", text)
	}
}

func TestHermeticGrokSessionLoad(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)

	agent, err := Start(Config{
		Provider:    ProviderGrok,
		WorkDir:     t.TempDir(),
		SessionID:   "sess-resume-me",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() != "sess-resume-me" {
		t.Fatalf("SessionID = %q, want sess-resume-me", agent.SessionID())
	}
}

func TestGrokAgentBackendCapabilities(t *testing.T) {
	caps := grokAgentBackend{}.Capabilities()
	if !caps.Task || !caps.Session || !caps.Resume {
		t.Fatalf("capabilities = %+v, want Task+Session+Resume", caps)
	}
}

// TestGrokSessionLiveSmoke exercises real grok agent stdio. Opt-in only.
func TestGrokSessionLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveGrokBin(); err != nil {
		t.Skipf("grok binary not found: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	text, err := Run(ctx, "Reply with exactly: pong", Config{
		Provider:    ProviderGrok,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("response %q, want pong", text)
	}
}

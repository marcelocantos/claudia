// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCursorACPCloseKillsAfterReadLoopClosed(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	c := &cursorACPClient{cmd: cmd, ownsProcess: true, closed: true}
	c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid=").Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Close left pid %d alive after readLoop already marked closed", pid)
}

func writeFakeCursorACP(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Cursor ACP uses a POSIX shell wrapper")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake ACP server")
	}
	py, err := filepath.Abs("testdata/cursor/acp/fake_acp.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	script := "#!/bin/sh\n" +
		"# Ignore agent/acp flags; speak ACP on stdio.\n" +
		"exec python3 \"" + py + "\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestHermeticCursorSessionStartSendWait(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	workDir := t.TempDir()
	agent, err := Start(Config{
		Provider:    ProviderCursor,
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
	if agent.PID() <= 0 {
		t.Fatal("cursor ACP start must record the child PID (🎯T541.1)")
	}

	ctx := t.Context()
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
}

func TestHermeticCursorSessionRunHelper(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	ctx := t.Context()
	text, err := Run(ctx, "Reply with exactly: pong", Config{
		Provider:    ProviderCursor,
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

func TestHermeticCursorSessionLoad(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	agent, err := Start(Config{
		Provider:    ProviderCursor,
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

func TestHermeticCursorLoadSurvivesMultiMegabyteJSONLine(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_HUGE_LOAD", "1")

	agent, err := Start(Config{
		Provider:      ProviderCursor,
		WorkDir:       t.TempDir(),
		SessionID:     "sess-huge-replay",
		RequireResume: true,
		TermLogPath:   "-",
	})
	if err != nil {
		t.Fatalf("session/load with a >1MiB JSON-RPC line: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() != "sess-huge-replay" {
		t.Fatalf("session=%q", agent.SessionID())
	}
}

func TestHermeticCursorLoadFailsClosedWhenRequireResume(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	agent, err := Start(Config{
		Provider:      ProviderCursor,
		WorkDir:       t.TempDir(),
		SessionID:     "sess-exists",
		RequireResume: true,
		TermLogPath:   "-",
	})
	if err == nil {
		agent.Stop()
		t.Fatal("Start must fail closed when load fails for an existing conversation")
	}
	if !strings.Contains(err.Error(), "refusing to mint a replacement session") {
		t.Fatalf("error %q lacks the fail-closed explanation", err)
	}
	if !IsCursorResumeDenied(err) {
		t.Fatalf("error %v is not ErrCursorResumeDenied", err)
	}
}

func TestHermeticCursorLoadFailsClosedWhenStoreExists(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "sess-has-store"
	dir := filepath.Join(home, ".cursor", "acp-sessions", sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "store.db"), []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		SessionID:   sid,
		TermLogPath: "-",
	})
	if err == nil {
		agent.Stop()
		t.Fatal("Start must not session/new when store.db already exists")
	}
	if !strings.Contains(err.Error(), "refusing to mint a replacement session") {
		t.Fatalf("error %q", err)
	}
	if !IsCursorResumeDenied(err) {
		t.Fatalf("error %v is not ErrCursorResumeDenied", err)
	}
}

func TestHermeticCursorLoadFallsThroughForMintedID(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		SessionID:   "sess-never-materialized",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start should mint a new session for an unmaterialized id: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() == "" {
		t.Fatal("empty session id after session/new fallback")
	}
	if agent.SessionID() == "sess-never-materialized" {
		t.Fatal("fake rejected load but id unchanged — fallback did not run")
	}
}

func TestHermeticCursorHyphenPermissionOptionID(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_CURSOR_PERMISSION", "1")

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	ctx := t.Context()
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

	if err := agent.Send("run a tool then reply pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for response (permission round-trip stuck?)")
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("WaitForResponse: %v", out.err)
		}
		if !strings.Contains(out.text, "pong") {
			t.Fatalf("response %q, want pong after hyphen permission grant", out.text)
		}
	}
}

func TestHermeticCursorAskQuestionDoesNotStall(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_CURSOR_ASK", "1")

	text, err := Run(t.Context(), "Reply with exactly: pong", Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("Run text %q, want pong after skipped ask_question", text)
	}
}

func TestCursorAgentBackendCapabilities(t *testing.T) {
	caps := cursorAgentBackend{}.Capabilities()
	if caps.Task || !caps.Session || !caps.Resume {
		t.Fatalf("capabilities = %+v, want Session+Resume only", caps)
	}
}

func TestCursorTaskBackendCapabilities(t *testing.T) {
	caps := cursorTaskBackend{}.Capabilities()
	if !caps.Task || !caps.Resume || caps.Session {
		t.Fatalf("capabilities = %+v, want Task+Resume", caps)
	}
}

func TestCursorRewindFailsWithCapabilityError(t *testing.T) {
	agent := &Agent{provider: ProviderCursor}
	_, err := agent.Rewind(1, Config{Provider: ProviderCursor})
	if err == nil {
		t.Fatal("Cursor Rewind returned nil error")
	}
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("error = %T %v, want CapabilityError", err, err)
	}
	if capErr.Provider != ProviderCursor || capErr.Capability != CapabilityRewind {
		t.Errorf("CapabilityError = %+v", capErr)
	}
}

func TestCursorSessionLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
		t.Skip("CLAUDIA_CURSOR_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCursorBin(); err != nil {
		t.Skipf("cursor agent binary not found: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	text, err := Run(ctx, "Reply with exactly: pong", Config{
		Provider:    ProviderCursor,
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

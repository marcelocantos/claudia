// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFakeCodexAppServer(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Codex app-server uses a POSIX shell wrapper")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake app-server")
	}
	py, err := filepath.Abs("testdata/codex/app-server/fake_app_server.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nexec python3 \"" + py + "\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func writeFakeCodexSubscriptionAuth(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	body := `{"auth_mode":"chatgpt","tokens":{"access_token":"test-token"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDIA_CODEX_AUTH_PATH", path)
	t.Setenv("OPENAI_API_KEY", "")
}

func TestHermeticCodexSessionStartSendWait(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)

	agent, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     t.TempDir(),
		Model:       "gpt-5-codex",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if !strings.HasPrefix(agent.SessionID(), "thr_") {
		t.Fatalf("SessionID = %q, want a Codex thread id", agent.SessionID())
	}
	if got := agent.AttachCommand(); got != "" {
		t.Fatalf("AttachCommand = %q, want empty", got)
	}

	reply, err := func() (string, error) {
		ctx := t.Context()
		errCh := make(chan error, 1)
		replyCh := make(chan string, 1)
		go func() {
			s, e := agent.WaitForResponse(ctx)
			if e != nil {
				errCh <- e
				return
			}
			replyCh <- s
		}()
		waitForEventSubscribers(t, agent, 1)
		if err := agent.Send("hello"); err != nil {
			return "", err
		}
		select {
		case e := <-errCh:
			return "", e
		case s := <-replyCh:
			return s, nil
		}
	}()
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if reply != "Final answer." {
		t.Fatalf("reply = %q", reply)
	}
	usage := agent.Usage()
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestHermeticCodexRequireResumeFailsClosed(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	t.Setenv("FAKE_CODEX_REJECT_RESUME", "1")
	writeFakeCodexSubscriptionAuth(t)

	_, err := Start(Config{
		Provider:      ProviderCodex,
		WorkDir:       t.TempDir(),
		SessionID:     "thr_missing",
		RequireResume: true,
		TermLogPath:   "-",
	})
	if err == nil {
		t.Fatal("Start succeeded; want fail-closed resume")
	}
	if !strings.Contains(err.Error(), "refusing to mint") {
		t.Fatalf("err = %v, want refuse-to-mint", err)
	}
}

func TestHermeticCodexRequireResumeKeepsThreadID(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)

	const want = "thr_existing"
	agent, err := Start(Config{
		Provider:      ProviderCodex,
		WorkDir:       t.TempDir(),
		SessionID:     want,
		RequireResume: true,
		TermLogPath:   "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() != want {
		t.Fatalf("SessionID = %q, want %q", agent.SessionID(), want)
	}
}

func TestHermeticCodexUnmaterializedFallsThrough(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)

	agent, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     t.TempDir(),
		SessionID:   "not-a-thread",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() == "not-a-thread" {
		t.Fatal("unmaterialized id was not replaced by thread/start")
	}
	if !strings.HasPrefix(agent.SessionID(), "thr_") {
		t.Fatalf("SessionID = %q", agent.SessionID())
	}
}

func TestCodexSessionLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCodexBin(); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}

	agent, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() == "" {
		t.Fatal("empty thread id")
	}

	ctx := t.Context()
	errCh := make(chan error, 1)
	replyCh := make(chan string, 1)
	go func() {
		s, e := agent.WaitForResponse(ctx)
		if e != nil {
			errCh <- e
			return
		}
		replyCh <- s
	}()
	waitForEventSubscribers(t, agent, 1)
	if err := agent.Send("Reply with exactly: ok"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case e := <-errCh:
		t.Fatalf("WaitForResponse: %v", e)
	case s := <-replyCh:
		if s == "" {
			t.Fatal("empty reply")
		}
	}
}

func TestHermeticCodexInterrupt(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)

	agent, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	// Seed a turn id by starting a turn, then interrupt. The fake
	// answers the turn immediately; interrupt still speaks the method.
	waitForEventSubscribers(t, agent, 0)
	if err := agent.Send("go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := agent.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
}

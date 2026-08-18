// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
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
	if agent.SessionID() == "" {
		t.Fatal("empty SessionID after thread/start")
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

func TestCodexThreadStartParamsSandboxDefaultAndOverride(t *testing.T) {
	def := codexThreadStartParams(agentStartRequest{})
	if def.Sandbox != "read-only" {
		t.Fatalf("default sandbox = %q, want read-only", def.Sandbox)
	}
	got := codexThreadStartParams(agentStartRequest{Config: Config{SandboxMode: "workspace-write"}})
	if got.Sandbox != "workspace-write" {
		t.Fatalf("writable sandbox = %q, want workspace-write", got.Sandbox)
	}
}

func TestHermeticCodexStartHonoursSandboxMode(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)

	t.Run("default-read-only", func(t *testing.T) {
		last := filepath.Join(t.TempDir(), "start.json")
		t.Setenv("FAKE_CODEX_LAST_START", last)
		agent, err := Start(Config{
			Provider:    ProviderCodex,
			WorkDir:     t.TempDir(),
			TermLogPath: "-",
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer agent.Stop()
		raw, err := os.ReadFile(last)
		if err != nil {
			t.Fatalf("read start params: %v", err)
		}
		if !strings.Contains(string(raw), `"sandbox":"read-only"`) {
			t.Fatalf("default thread/start = %s", raw)
		}
	})

	t.Run("workspace-write", func(t *testing.T) {
		last := filepath.Join(t.TempDir(), "start.json")
		t.Setenv("FAKE_CODEX_LAST_START", last)
		agent, err := Start(Config{
			Provider:    ProviderCodex,
			WorkDir:     t.TempDir(),
			SandboxMode: "workspace-write",
			TermLogPath: "-",
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer agent.Stop()
		raw, err := os.ReadFile(last)
		if err != nil {
			t.Fatalf("read start params: %v", err)
		}
		if !strings.Contains(string(raw), `"sandbox":"workspace-write"`) {
			t.Fatalf("writable thread/start = %s", raw)
		}
	})
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
	if agent.SessionID() == "" {
		t.Fatal("empty SessionID after fall-through mint")
	}
}

func TestHermeticCodexResumesNonThrPrefix(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	const want = "01a00f11-547e-7a32-a284-b5832f3697db"
	t.Setenv("FAKE_CODEX_RESUME_ID", want)
	writeFakeCodexSubscriptionAuth(t)

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

func TestCodexSessionLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCodexBin(); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}

	workDir := t.TempDir()
	first, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     workDir,
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	threadID := first.SessionID()
	if threadID == "" {
		first.Stop()
		t.Fatal("empty thread id")
	}
	if err := liveCodexTurn(t, first, "Reply with exactly: ok"); err != nil {
		first.Stop()
		t.Fatal(err)
	}
	first.Stop()

	second, err := Start(Config{
		Provider:      ProviderCodex,
		WorkDir:       workDir,
		SessionID:     threadID,
		RequireResume: true,
		TermLogPath:   "-",
	})
	if err != nil {
		t.Fatalf("resume Start: %v", err)
	}
	defer second.Stop()
	if second.SessionID() != threadID {
		t.Fatalf("resumed SessionID = %q, want %q", second.SessionID(), threadID)
	}
	if err := liveCodexTurn(t, second, "Reply with exactly: ok"); err != nil {
		t.Fatal(err)
	}
}

func liveCodexTurn(t *testing.T, agent *Agent, prompt string) error {
	t.Helper()
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
	if err := agent.Send(prompt); err != nil {
		return err
	}
	select {
	case e := <-errCh:
		return e
	case s := <-replyCh:
		if s == "" {
			return fmt.Errorf("empty reply")
		}
		return nil
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

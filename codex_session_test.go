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
	"time"
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
	script := "#!/bin/sh\nexec python3 \"" + py + "\" \"$@\"\n"
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

func TestCodexSessionLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (this test spends API credit)")
	}
	// The real path since 🎯T178: a Codex Session seat on the sidecar.

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

// TestHermeticCodexThreadStartTimesOut drives startCodexAppServer, the
// handshake client that still owns codexAppServerThreadTimeout. Session
// StartAgent no longer reaches it. The bound guard requires a hermetic
// assignment of that var, and the error names the step that expired.
func TestHermeticCodexThreadStartTimesOut(t *testing.T) {
	const threadBound = 200 * time.Millisecond
	prev := codexAppServerThreadTimeout
	codexAppServerThreadTimeout = threadBound
	t.Cleanup(func() { codexAppServerThreadTimeout = prev })

	bin := writeFakeCodexAppServer(t)
	t.Setenv("FAKE_CODEX_HANG_START", "1")

	client, err := startCodexAppServer(bin, t.TempDir(), "", "", false, "", codexSandboxTuning{}, nil, nil, nil)
	if client != nil {
		client.Close()
	}
	if err == nil {
		t.Fatal("start succeeded against a hanging thread/start")
	}
	want := "timeout waiting for thread/start after " + threadBound.String()
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("start err = %v, want %q", err, want)
	}
}

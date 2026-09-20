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

// 🎯T545.1.2: exclusive CODEX_HOME survives Stop so bounce can thread/resume.
func TestHermeticCodexExclusiveHomeSurvivesStop(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lastHome := filepath.Join(t.TempDir(), "home1")
	t.Setenv("FAKE_CODEX_LAST_HOME", lastHome)

	minted := "fd4bbbe8-0000-4000-8000-000000d95306"
	cfg := Config{
		Provider:     ProviderCodex,
		WorkDir:      t.TempDir(),
		SessionID:    minted,
		MCPExclusive: true,
		MCPServers:   []MCPServer{{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"}},
		TermLogPath:  "-",
	}
	agent, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := agent.SessionID()
	if sid == "" {
		agent.Stop()
		t.Fatal("empty SessionID")
	}
	durable := exclusiveCodexHomeDir(sid)
	if !dirExists(durable) {
		agent.Stop()
		t.Fatal("durable home missing before Stop — SIGHUP skips StopAll")
	}
	agent.Stop()
	if !dirExists(durable) {
		t.Fatalf("durable home deleted on Stop: %s", durable)
	}

	lastHome2 := filepath.Join(t.TempDir(), "home2")
	t.Setenv("FAKE_CODEX_LAST_HOME", lastHome2)
	cfg.SessionID = sid
	cfg.RequireResume = true
	agent2, err := Start(cfg)
	if err != nil {
		t.Fatalf("resume Start: %v", err)
	}
	defer agent2.Stop()
	if agent2.SessionID() != sid {
		t.Fatalf("resumed SessionID = %q, want %q", agent2.SessionID(), sid)
	}
	secondHome, err := os.ReadFile(lastHome2)
	if err != nil {
		t.Fatalf("resume CODEX_HOME: %v", err)
	}
	if strings.TrimSpace(string(secondHome)) != durable {
		t.Fatalf("resume CODEX_HOME = %q, want durable %q", secondHome, durable)
	}
}

func TestHermeticCodexExclusiveHomeMissingFailsLoud(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	_, err := Start(Config{
		Provider:      ProviderCodex,
		WorkDir:       t.TempDir(),
		SessionID:     "thr_existing",
		RequireResume: true,
		MCPExclusive:  true,
		TermLogPath:   "-",
	})
	if err == nil {
		t.Fatal("Start succeeded with missing exclusive home")
	}
	want := exclusiveCodexHomeDir("thr_existing")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want named home %s", err, want)
	}
	if strings.Contains(err.Error(), "no rollout found") {
		t.Fatal("must fail before empty-home resume")
	}
}

func TestHermeticCodexCloseFlushesExclusiveHome(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	agent, err := Start(Config{
		Provider:     ProviderCodex,
		WorkDir:      t.TempDir(),
		SessionID:    "fd4bbbe8-0000-4000-8000-000000d95307",
		MCPExclusive: true,
		MCPServers:   []MCPServer{{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"}},
		TermLogPath:  "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	home := exclusiveCodexHomeDir("fd4bbbe8-0000-4000-8000-000000d95307")
	agent.Stop()
	if _, err := os.Stat(filepath.Join(home, "flushed")); err != nil {
		t.Fatalf("Stop SIGKILL-ed Codex (no SIGTERM flush in exclusive home %s): %v", home, err)
	}
}

func TestHermeticCodexStartSeedsRolloutWithoutTurn(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	t.Setenv("FAKE_CODEX_SKIP_ROLLOUT", "1")
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	agent, err := Start(Config{
		Provider:     ProviderCodex,
		WorkDir:      t.TempDir(),
		MCPExclusive: true,
		MCPServers:   []MCPServer{{Name: "onlyme", URL: "http://127.0.0.1:9/mcp"}},
		TermLogPath:  "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sid := agent.SessionID()
	if sid == "" {
		agent.Stop()
		t.Fatal("empty SessionID")
	}
	home := exclusiveCodexHomeDir(sid)
	if findCodexRollout(home, sid) == "" {
		agent.Stop()
		t.Fatalf("no thread/name/set rollout under %s", home)
	}
	agent.Stop()

	agent2, err := Start(Config{
		Provider:      ProviderCodex,
		WorkDir:       t.TempDir(),
		SessionID:     sid,
		RequireResume: true,
		MCPExclusive:  true,
		TermLogPath:   "-",
	})
	if err != nil {
		t.Fatalf("resume after name/set persist: %v", err)
	}
	defer agent2.Stop()
	if agent2.SessionID() != sid {
		t.Fatalf("resumed SessionID = %q, want %q", agent2.SessionID(), sid)
	}
}

func TestHermeticCodexResumeEmptyHomeNamesPath(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	sid := "01a0320f-0000-4000-8000-00000000dead"
	home := exclusiveCodexHomeDir(sid)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Start(Config{
		Provider:      ProviderCodex,
		WorkDir:       t.TempDir(),
		SessionID:     sid,
		RequireResume: true,
		MCPExclusive:  true,
		TermLogPath:   "-",
	})
	if err == nil {
		t.Fatal("Start resumed an empty exclusive home")
	}
	if !strings.Contains(err.Error(), home) {
		t.Fatalf("err = %v, want named home %s", err, home)
	}
	if !strings.Contains(err.Error(), "refusing to mint") {
		t.Fatalf("err = %v, want refuse remint", err)
	}
}

// 🎯T545.1.1: a thread/start that never replies must fail loud inside the
// handshake window, not block the caller's MCP tools/call until the client
// gives up.
//
// 🎯T93: only the thread bound is shortened. This test once moved the one
// knob that bounded the whole handshake, so at load average 122 the fake
// peer's initialize — a python process the host had not got round to
// scheduling — expired first and Start failed a step earlier than the
// subject. Leaving codexAppServerInitializeTimeout at its production 20s
// means a loaded host would have to be 100x slower than the bound this
// test arms before it could answer for the wrong step again.
//
// The assertion reads both halves of the error, the step and the duration,
// because the step alone does not say which bound produced it. There is no
// elapsed-time assertion: the message naming 200ms is the proof the
// handshake window closed, and `go test -timeout` is the only clock
// allowed to decide a hermetic verdict (🎯T33).
func TestHermeticCodexThreadStartTimesOut(t *testing.T) {
	const threadBound = 200 * time.Millisecond
	prev := codexAppServerThreadTimeout
	codexAppServerThreadTimeout = threadBound
	t.Cleanup(func() { codexAppServerThreadTimeout = prev })

	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	t.Setenv("FAKE_CODEX_HANG_START", "1")
	writeFakeCodexSubscriptionAuth(t)

	_, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err == nil {
		t.Fatal("Start succeeded against a hanging thread/start")
	}
	want := "timeout waiting for thread/start after " + threadBound.String()
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Start err = %v, want %q", err, want)
	}
}

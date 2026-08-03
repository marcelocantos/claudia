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
	// Hermetic fakes speak ACP on stdio; ambient CLAUDIA_GROK_CONNECT=1
	// (fleet shells) would force connect/serve and fail these tests.
	t.Setenv(EnvGrokConnect, "")
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

// TestHermeticGrokBashPermissionOptionID proves the client never replies
// with a foreign optionId when Grok only offers bash-scoped allows
// (live failure: unknown permission option for tool run_terminal_command).
func TestHermeticGrokBashPermissionOptionID(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)
	t.Setenv("FAKE_ACP_BASH_PERMISSION", "1")

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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

	if err := agent.Send("run a shell command then reply pong"); err != nil {
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
			t.Fatalf("response %q, want pong after bash permission grant", out.text)
		}
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

// Fail-closed load (jevons 🎯T30.1): when the caller marks the session
// id as an existing conversation (RequireResume), a failed session/load
// must error out rather than silently minting a replacement session —
// that silent fallback is how a conversation gets lost.
func TestHermeticGrokLoadFailsClosedWhenRequireResume(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	agent, err := Start(Config{
		Provider:      ProviderGrok,
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
}

// Without RequireResume, a locally minted id may still fall through to
// session/new — the legitimate first-launch path.
func TestHermeticGrokLoadFallsThroughForMintedID(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	agent, err := Start(Config{
		Provider:    ProviderGrok,
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

// With MCP configured, resume must rotate (session/new) rather than load —
// Grok ignores mcpServers on session/load. Fake rejects load so a mistaken
// load path would fail Start; rotation must succeed with a new id.
func TestHermeticGrokTooledResumeRotates(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	dir := t.TempDir()
	mcpPath := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"x":{"type":"http","url":"http://127.0.0.1:9/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	agent, err := Start(Config{
		Provider:    ProviderGrok,
		WorkDir:     dir,
		SessionID:   "sess-old-tooled",
		MCPConfig:   mcpPath,
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start should rotate to session/new with tools: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() == "sess-old-tooled" {
		t.Fatal("expected new session id after tooled rotation")
	}
	if agent.SessionID() == "" {
		t.Fatal("empty session id")
	}
}

// RequireResume + MCP still rotates (tools over same-id load). Hosts that
// track SessionID (Registry) pick up the new id after Start.
func TestHermeticGrokRequireResumeWithMCPStillRotates(t *testing.T) {
	bin := writeFakeGrokACP(t)
	t.Setenv("GROK_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	dir := t.TempDir()
	mcpPath := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"x":{"type":"http","url":"http://127.0.0.1:9/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	agent, err := Start(Config{
		Provider:      ProviderGrok,
		WorkDir:       dir,
		SessionID:     "sess-exists",
		RequireResume: true,
		MCPConfig:     mcpPath,
		TermLogPath:   "-",
	})
	if err != nil {
		t.Fatalf("Start should rotate with tools even under RequireResume: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() == "sess-exists" {
		t.Fatal("expected rotated session id when MCP is configured")
	}
}

// grok agent stdio loads MCP servers ONLY from the ACP session param —
// this pins the .mcp.json → ACP conversion that gives agents their tools.
func TestACPMCPServersConversion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	cfg := `{"mcpServers":{
		"jevons":{"type":"http","url":"http://127.0.0.1:13705/mcp"},
		"bridge":{"command":"/usr/local/bin/mcpbridge","args":["connect","x.json"],"env":{"A":"1"}}}}`
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out := acpMCPServers(path)
	if len(out) != 2 {
		t.Fatalf("got %d servers, want 2: %v", len(out), out)
	}
	byName := map[string]map[string]any{}
	for _, e := range out {
		m := e.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if byName["jevons"]["type"] != "http" || byName["jevons"]["url"] != "http://127.0.0.1:13705/mcp" {
		t.Fatalf("http entry wrong: %v", byName["jevons"])
	}
	if byName["bridge"]["command"] != "/usr/local/bin/mcpbridge" {
		t.Fatalf("stdio entry wrong: %v", byName["bridge"])
	}
	if got := acpMCPServers(filepath.Join(dir, "missing.json")); got != nil {
		t.Fatalf("missing file should yield nil, got %v", got)
	}
	if got := acpMCPServers(""); got != nil {
		t.Fatalf("empty path should yield nil, got %v", got)
	}
}

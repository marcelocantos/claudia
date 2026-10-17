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

func TestGrokACPEventsCarryPromptBracketIdentity(t *testing.T) {
	var got []Event
	c := &grokACPClient{
		sessionID: "sess-identity",
		prompts:   acpPromptStack{ids: []int64{41}},
		onEvent:   func(ev Event) { got = append(got, ev) },
	}
	update := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-identity","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"chunk"}}}}`)

	c.dispatchMessage(update)
	c.dispatchMessage([]byte(`{"jsonrpc":"2.0","id":41,"result":{"stopReason":"end_turn"}}`))
	// Once the response closes prompt 41, a stray update remains associated
	// with its session but must not inherit the completed prompt id.
	c.dispatchMessage(update)
	c.mu.Lock()
	c.prompts = acpPromptStack{ids: []int64{42}}
	c.mu.Unlock()
	c.dispatchMessage(update)
	c.dispatchMessage([]byte(`{"jsonrpc":"2.0","id":42,"result":{"stopReason":"end_turn"}}`))
	c.mu.Lock()
	c.prompts = acpPromptStack{ids: []int64{43}}
	c.mu.Unlock()
	c.dispatchMessage([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-other","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"foreign"}}}}`))

	if len(got) != 6 {
		t.Fatalf("events = %d, want 6: %+v", len(got), got)
	}
	wantTurns := []string{"41", "41", "", "42", "42", ""}
	for i, want := range wantTurns {
		wantSession := "sess-identity"
		if i == 5 {
			wantSession = "sess-other"
		}
		if got[i].SessionID != wantSession || got[i].TurnID != want {
			t.Errorf("event[%d] identity = session %q turn %q, want %s/%q", i, got[i].SessionID, got[i].TurnID, wantSession, want)
		}
	}
	if !got[1].IsTerminalStop() || !got[4].IsTerminalStop() {
		t.Fatalf("terminal events = [%v, %v], want true", got[1].IsTerminalStop(), got[4].IsTerminalStop())
	}
	if got[0].TurnID == got[3].TurnID {
		t.Fatalf("consecutive Grok prompts share TurnID %q", got[0].TurnID)
	}
	if got[5].SessionID != "sess-other" {
		t.Fatalf("foreign update SessionID = %q, want sess-other", got[5].SessionID)
	}
}

func TestGrokAgentBackendCapabilities(t *testing.T) {
	caps := grokAgentBackend{}.Capabilities()
	if caps.Task || !caps.Session || !caps.Resume {
		t.Fatalf("capabilities = %+v, want Session+Resume without Task", caps)
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

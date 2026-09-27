// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

var ompSidecarIDs = []Provider{
	Provider(omp.Anthropic),
	Provider(omp.OpenAICodex),
	ProviderCursor,
	Provider(omp.XAIOAuth),
}

func TestT869AdoptOnlyDoesNotStartASidecar(t *testing.T) {
	t.Setenv(omp.SocketEnv, "")
	_, err := (ompAgentBackend{}).StartAgent(agentStartRequest{
		Context: context.Background(),
		Config:  Config{Provider: ProviderGrok, Name: "jevons", AdoptOnly: true},
	})
	if !errors.Is(err, ErrNoSessionWindow) {
		t.Fatalf("adopt with no sidecar = %v, want ErrNoSessionWindow", err)
	}
}

func TestOMPStartRefusesVendorCLI(t *testing.T) {
	t.Setenv("CLAUDIA_OMP_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("The specified item could not be found in the keychain.")
	}
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("login not available in test")
		},
	}
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := append(append([]Provider{}, ompSidecarIDs...), ProviderGrok)
	for _, p := range ids {
		cfg := Config{Provider: p, WorkDir: t.TempDir(), TermLogPath: "-"}
		_, err := StartDirect(cfg)
		if err == nil {
			t.Fatalf("%s started a vendor CLI", p)
		}
		if strings.Contains(err.Error(), "tmux") || strings.Contains(err.Error(), "grok agent") || strings.Contains(err.Error(), "app-server") {
			t.Fatalf("%s fell through to a vendor CLI: %v", p, err)
		}
	}
}

func TestUseOMPIncludesGrokAndCursorWithoutFlag(t *testing.T) {
	if !useOMP(Config{Provider: ProviderGrok}) {
		t.Fatal("grok must use the sidecar as xai-oauth")
	}
	if ompProviderID(ProviderGrok) != omp.XAIOAuth {
		t.Fatalf("grok maps to %s, want %s", ompProviderID(ProviderGrok), omp.XAIOAuth)
	}
	if !useOMP(Config{Provider: ProviderCursor}) {
		t.Fatal("cursor must use the sidecar without Config.OMP")
	}
	if useOMP(Config{Provider: ProviderBedrock}) {
		t.Fatal("bedrock is not a subscription sidecar seat")
	}
	if useOMP(Config{Provider: ProviderClaude}) || useOMP(Config{Provider: ProviderCodex}) {
		t.Fatal("claude/codex CLI ids stay vendor until reminted onto anthropic/openai-codex")
	}
}

func TestOMPStartLoadsTokenFromKeychain(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-omp-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan omp.Message, 4)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var msg omp.Message
			_ = json.Unmarshal(line, &msg)
			got <- msg
			if msg.Op == omp.OpLoad {
				_, _ = c.Write([]byte("{\"type\":\"ready\"}\n"))
			}
			if msg.Op == omp.OpPrompt {
				_, _ = c.Write([]byte("{\"type\":\"text\",\"text\":\"hi\"}\n"))
				_, _ = c.Write([]byte("{\"type\":\"turn_end\",\"snapshot\":{\"messages\":[]}}\n"))
			}
		}
	}()

	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"r","access_token":"plan-token","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(blob), nil
	}
	t.Cleanup(func() { ompKeychain = nil; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)

	agent, err := StartDirect(Config{
		Name:        "seat",
		Provider:    Provider(omp.Anthropic),
		Model:       "claude-opus",
		WorkDir:     dir,
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	events := make(chan Event, 8)
	agent.SubscribeEvents(func(ev Event) {
		select {
		case events <- ev:
		default:
		}
	})
	msg := <-got
	if msg.Op != omp.OpLoad || msg.Token != "plan-token" || msg.Provider != omp.Anthropic || msg.Cwd != dir {
		t.Fatalf("load = %+v", msg)
	}
	if err := agent.Send("hello"); err != nil {
		t.Fatal(err)
	}
	prompt := <-got
	if prompt.Op != omp.OpPrompt || prompt.Text != "hello" {
		t.Fatalf("prompt = %+v", prompt)
	}
	if prompt.Cause != omp.CauseOwner || prompt.TurnID == "" || prompt.SessionID == "" || prompt.CauseDetail != "hello" {
		t.Fatalf("prompt did not name an owner turn: %+v", prompt)
	}
	agent.SetPromptCause(PromptCause{Cause: omp.CauseRestartNudge, Resume: "launched", Detail: "host restarted at 11:37:34"})
	if err := agent.Send("[claudia] The host restarted at 11:37:34"); err != nil {
		t.Fatal(err)
	}
	nudged := <-got
	if nudged.Cause != omp.CauseRestartNudge || nudged.Resume != "launched" || nudged.SessionID != prompt.SessionID {
		t.Fatalf("restart nudge = %+v, want cause restart-nudge resume=launched session %s", nudged, prompt.SessionID)
	}
	deadline := time.After(2 * time.Second)
	var sawText, sawEnd bool
	for !sawText || !sawEnd {
		select {
		case ev := <-events:
			if ev.Type == "assistant" && ev.Text == "hi" {
				sawText = true
			}
			if ev.StopReason == "end_turn" {
				sawEnd = true
			}
		case <-deadline:
			t.Fatalf("event stream text=%v turn_end=%v", sawText, sawEnd)
		}
	}
}

func TestOMPStartRefreshesExpiredToken(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-omp-exp-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan omp.Message, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var msg omp.Message
		_ = json.Unmarshal(line, &msg)
		got <- msg
		_, _ = c.Write([]byte("{\"type\":\"ready\"}\n"))
		select {}
	}()

	exp := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"old-r","access_token":"old","expiry":"` + exp + `"}}}`
	fresh := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ompKeychain = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
			return []byte(blob), nil
		}
		return nil, nil
	}
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) < 3 || args[1] != "refresh" || args[2] != omp.Anthropic {
				t.Fatalf("login args = %v", args)
			}
			return []byte(`{"refresh_token":"new-r","access_token":"fresh-token","expiry":"` + fresh + `"}`), nil
		},
	}
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)

	agent, err := StartDirect(Config{
		Name:        "seat",
		Provider:    Provider(omp.Anthropic),
		WorkDir:     dir,
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	msg := <-got
	if msg.Token != "fresh-token" {
		t.Fatalf("load token = %q", msg.Token)
	}
}

func TestOMPStartRefreshFailureDoesNotStart(t *testing.T) {
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("The specified item could not be found in the keychain.")
	}
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("oauth refresh rejected")
		},
	}
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDIA_OMP_SOCKET", filepath.Join(t.TempDir(), "unused.sock"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-be-used")
	t.Setenv("OPENAI_API_KEY", "sk-should-not-be-used")
	t.Setenv("XAI_API_KEY", "sk-should-not-be-used")

	_, err := StartDirect(Config{
		Provider:    Provider(omp.Anthropic),
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err == nil || !strings.Contains(err.Error(), "refresh failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestOMPMigrateUsesSidecarNotVendorCLI(t *testing.T) {
	t.Setenv("CLAUDIA_OMP_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("The specified item could not be found in the keychain.")
	}
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("login not available in test")
		},
	}
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	src, _ := startMigrateFixture(t, ProviderGrok, "fake-grok")
	src.PublishEvent(Event{Type: "user", Text: "hello"})
	src.PublishEvent(Event{Type: "assistant", Text: "hi"})
	dests := []Provider{Provider(omp.Anthropic), Provider(omp.OpenAICodex), Provider(omp.XAIOAuth), ProviderCursor}
	for _, dest := range dests {
		err := src.Migrate(&MigrateArgs{Provider: dest, Force: true})
		if err == nil {
			t.Fatalf("%s migrate started a seat without credentials", dest)
		}
		if strings.Contains(err.Error(), "tmux") || strings.Contains(err.Error(), "grok agent") || strings.Contains(err.Error(), "app-server") {
			t.Fatalf("%s migrate fell through to a vendor CLI: %v", dest, err)
		}
	}
}

func TestOMPSetModelIsSecondLoad(t *testing.T) {
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-omp-set-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan omp.Message, 4)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var msg omp.Message
			_ = json.Unmarshal(line, &msg)
			got <- msg
			if msg.Op == omp.OpLoad {
				_, _ = c.Write([]byte("{\"type\":\"ready\"}\n"))
			}
		}
	}()

	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"r","access_token":"plan-token","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(blob), nil
	}
	t.Cleanup(func() { ompKeychain = nil; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)

	agent, err := StartDirect(Config{
		Name:        "seat",
		Provider:    Provider(omp.Anthropic),
		Model:       "claude-opus",
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if load := <-got; load.Op != omp.OpLoad || load.Model != "claude-opus" {
		t.Fatalf("first load = %+v", load)
	}
	if err := agent.SetModel("claude-sonnet"); err != nil {
		t.Fatal(err)
	}
	second := <-got
	if second.Op != omp.OpLoad || second.Model != "claude-sonnet" || second.Token != "plan-token" {
		t.Fatalf("setModel load = %+v", second)
	}
	if second.Text != "" {
		t.Fatal("setModel must not send a second seed")
	}
}

func TestOMPSidecarPromptCallsPiAgentCore(t *testing.T) {
	seat, err := os.ReadFile("sidecar/seat.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(seat)
	if !strings.Contains(src, `from "@oh-my-pi/pi-agent-core"`) {
		t.Fatal("seat.ts must import the pinned @oh-my-pi/pi-agent-core package")
	}
	if !strings.Contains(src, "agent.prompt(") {
		t.Fatal("a prompt must call Agent.prompt on pi-agent-core")
	}
	if !strings.Contains(src, "type: \"turn_end\"") || !strings.Contains(src, "snapshot: agent.state") {
		t.Fatal("turn_end must snapshot that Agent's context")
	}
	if !strings.Contains(src, `from "./turn.ts"`) || !strings.Contains(src, "stop_token") {
		t.Fatal("seat.ts must write a turn digest and keep a stop token off the text stream")
	}
	if strings.Contains(src, `@oh-my-pi/pi-natives`) || strings.Contains(src, `@oh-my-pi/pi-coding-agent`) {
		t.Fatal("sidecar must not load pi-natives or omp's tools")
	}
	if !strings.Contains(src, `from "./coding.ts"`) || !strings.Contains(src, "setTools(opts.summaryOnly ? [] : codingTools") {
		t.Fatal("seat.ts must advertise host coding tools (Bash/Read/Write/Glob/Grep)")
	}
	coding, err := os.ReadFile("sidecar/coding.ts")
	if err != nil {
		t.Fatal(err)
	}
	csrc := string(coding)
	for _, name := range []string{`"Bash"`, `"Read"`, `"Write"`, `"Glob"`, `"Grep"`} {
		if !strings.Contains(csrc, name) {
			t.Fatalf("coding.ts missing %s", name)
		}
	}
	server, err := os.ReadFile("sidecar/server.ts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(server), `from "./spool.ts"`) {
		t.Fatal("server.ts must write the dated spool")
	}
	pkg, err := os.ReadFile("sidecar/package.json")
	if err != nil {
		t.Fatal(err)
	}
	body := string(pkg)
	if !strings.Contains(body, `"@oh-my-pi/pi-agent-core": "18.2.11"`) ||
		!strings.Contains(body, `"@oh-my-pi/pi-ai": "18.2.11"`) {
		t.Fatalf("package versions must be pinned to 18.2.11, not main: %s", body)
	}
	auth, err := os.ReadFile("sidecar/auth.ts")
	if err != nil {
		t.Fatal(err)
	}
	as := string(auth)
	if !strings.Contains(as, `from "@oh-my-pi/pi-ai"`) {
		t.Fatal("auth.ts must call pi-ai, not a Go OAuth client")
	}
	if !strings.Contains(as, "refreshOAuthToken") || !strings.Contains(as, "getProviderDefinition") {
		t.Fatal("auth.ts must use pi-ai login or refresh")
	}
}

func TestOMPGrantCarriesOMP(t *testing.T) {
	def := configToGrantDef("seat", Config{Provider: ProviderCursor, OMP: true, SummaryOnly: true, WorkDir: "/w"}, nil)
	if !def.OMP {
		t.Fatal("grant must persist Config.OMP so a bounce does not start the vendor CLI")
	}
	cfg := def.Config()
	if !cfg.OMP || !cfg.SummaryOnly || cfg.Provider != ProviderCursor {
		t.Fatalf("rehydrated cfg = %+v", cfg)
	}
}

func TestExecKeychainACLTimeoutDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := execKeychain(ctx, "sleep", "5")
	if err == nil {
		t.Fatal("timed-out keychain read must fail")
	}
	if !strings.Contains(err.Error(), "keychain ACL did not approve this binary") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("ACL refusal hung for %s", time.Since(start))
	}
}

func TestOMPExecScrubsEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-live")
	t.Setenv("OPENAI_API_KEY", "sk-live")
	t.Setenv("XAI_API_KEY", "sk-live")
	t.Setenv("CURSOR_ACCESS_TOKEN", "tok-live")
	out, err := execKeychain(context.Background(), "/usr/bin/env")
	if err != nil {
		t.Fatal(err)
	}
	blob := string(out)
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "CURSOR_ACCESS_TOKEN"} {
		if strings.Contains(blob, name+"=") {
			t.Fatalf("execKeychain env still has %s", name)
		}
	}
	server, err := os.ReadFile("sidecar/server.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "CURSOR_ACCESS_TOKEN"} {
		if !strings.Contains(string(server), name) {
			t.Fatalf("server.ts must drop %s", name)
		}
	}
}

func TestCallJevonsMCPPostsToolsCall(t *testing.T) {
	var gotName string
	ln := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotName = req.Params.Name
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok-from-mcp"}]}}`))
	}))
	defer ln.Close()
	got := CallJevonsMCP(ln.URL, "jevons_agent_list", `{"query":"running"}`)
	if got != "ok-from-mcp" {
		t.Fatalf("result = %q", got)
	}
	if gotName != "jevons_agent_list" {
		t.Fatalf("called %q", gotName)
	}
	if got := CallJevonsMCP(ln.URL, "bash", "{}"); !strings.Contains(got, "refusing") {
		t.Fatalf("non-jevons tool: %q", got)
	}
}

func TestOMPLoginRunsBunFromSidecar(t *testing.T) {
	src, err := os.ReadFile("omp_agent.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "login.Run = execBunLogin") {
		t.Fatal("StartAgent must run auth.ts through bun, not the Keychain runner")
	}
	if !strings.Contains(body, "filepath.Dir(sidecarAuthScript())") || !strings.Contains(body, "cmd.Dir") {
		t.Fatal("bun login must run from the sidecar directory")
	}
	if !strings.Contains(body, "io.MultiWriter(os.Stderr, &stderr)") {
		t.Fatal("bun login must forward the auth URL on stderr")
	}
	if !oauthRejected("", json.RawMessage(`{"errorMessage":"403 The OAuth2 access token could not be validated."}`)) {
		t.Fatal("a rejected OAuth token must be noticed")
	}
	if !oauthRejected("401 invalid_token", nil) {
		t.Fatal("an error event that names invalid_token must refresh that plan")
	}
	if oauthRejected("", nil) || oauthRejected("", json.RawMessage(`{"stopReason":"stop"}`)) {
		t.Fatal("a normal turn must not look like a rejected token")
	}
	if oauthRejected("Agent is already processing", nil) {
		t.Fatal("a busy seat is not a rejected access token")
	}
	script := sidecarAuthScript()
	if _, err := os.Stat(filepath.Join(filepath.Dir(script), "package.json")); err != nil {
		t.Fatalf("sidecar package.json next to %s: %v", script, err)
	}
}

func TestOMPNoGoOAuthClient(t *testing.T) {
	banned := []string{
		"claude.ai/oauth",
		"auth.openai.com/oauth",
		"auth.x.ai",
		"accounts.cursor.com",
		"pkceS256",
	}
	check := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, tok := range banned {
			if strings.Contains(src, tok) {
				t.Errorf("%s contains %q; Claudia must not implement the OAuth dance", path, tok)
			}
		}
	}
	err := filepath.Walk("omp", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		check(path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	check("omp_agent.go")
}

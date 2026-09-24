// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
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

func TestOMPStartRefusesVendorCLI(t *testing.T) {
	t.Setenv("CLAUDIA_OMP_SOCKET", "")
	for _, p := range ompSidecarIDs {
		cfg := Config{Provider: p, WorkDir: t.TempDir(), TermLogPath: "-"}
		if p == ProviderCursor {
			cfg.OMP = true
		}
		_, err := StartDirect(cfg)
		if err == nil || !strings.Contains(err.Error(), "vendor CLI") {
			t.Fatalf("%s err = %v", p, err)
		}
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
	t.Cleanup(func() { ompKeychain = nil })
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
	msg := <-got
	if msg.Op != omp.OpLoad || msg.Token != "plan-token" || msg.Provider != omp.Anthropic {
		t.Fatalf("load = %+v", msg)
	}
	if err := agent.Send("hello"); err != nil {
		t.Fatal(err)
	}
	prompt := <-got
	if prompt.Op != omp.OpPrompt || prompt.Text != "hello" {
		t.Fatalf("prompt = %+v", prompt)
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
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{} })
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
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{} })
	t.Setenv("CLAUDIA_OMP_SOCKET", filepath.Join(t.TempDir(), "unused.sock"))

	_, err := StartDirect(Config{
		Provider:    Provider(omp.Anthropic),
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err == nil || !strings.Contains(err.Error(), "refresh failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestOMPMigrateRefusesVendorCLI(t *testing.T) {
	t.Setenv("CLAUDIA_OMP_SOCKET", "")
	src, _ := startMigrateFixture(t, ProviderGrok, "fake-grok")
	src.PublishEvent(Event{Type: "user", Text: "hello"})
	src.PublishEvent(Event{Type: "assistant", Text: "hi"})
	for _, dest := range []Provider{Provider(omp.Anthropic), Provider(omp.OpenAICodex), Provider(omp.XAIOAuth)} {
		err := src.Migrate(&MigrateArgs{Provider: dest, Force: true})
		if err == nil || !strings.Contains(err.Error(), "vendor CLI") {
			t.Fatalf("%s migrate err = %v", dest, err)
		}
	}
	src.startCfg.OMP = true
	err := src.Migrate(&MigrateArgs{Provider: ProviderCursor, Force: true})
	if err == nil || !strings.Contains(err.Error(), "vendor CLI") {
		t.Fatalf("cursor migrate err = %v", err)
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
	t.Cleanup(func() { ompKeychain = nil })
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
	if strings.Contains(src, `@oh-my-pi/pi-natives`) || strings.Contains(src, `@oh-my-pi/pi-coding-agent`) {
		t.Fatal("sidecar must not load pi-natives or omp's tools")
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

func TestOMPNoGoOAuthClient(t *testing.T) {
	banned := []string{
		"claude.ai/oauth",
		"auth.openai.com/oauth",
		"auth.x.ai",
		"accounts.cursor.com",
		"pkceS256",
	}
	err := filepath.Walk("omp", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		for _, tok := range banned {
			if strings.Contains(src, tok) {
				t.Errorf("%s contains %q; Claudia must not implement the OAuth dance", path, tok)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

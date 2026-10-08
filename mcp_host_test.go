// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func buildMCPStdioFixture(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mcpstdio")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/mcpstdio")
	cmd.Dir = moduleRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build mcpstdio fixture: %v\n%s", err, out)
	}
	return bin
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func testMCPHost(t *testing.T) *MCPHost {
	t.Helper()
	h, err := NewMCPHost(&MCPHostArgs{StateDir: t.TempDir(), ListenAddr: "127.0.0.1:0",
		ConsumerOwned: func(s MCPServer) bool { return strings.HasPrefix(s.Name, "jevonsmcp") }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestMCPHostAttachRewritesStdioAndLeavesJevonsmcp(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	in := []MCPServer{
		{Name: "fixture", Command: bin},
		{Name: "jevonsmcp", Type: "http", URL: "http://127.0.0.1:13705/mcp"},
		{Name: "jevonsmcp-journey", Type: "http", URL: "http://127.0.0.1:13715/mcp"},
	}
	got := h.Attach(in)
	if len(got) != 3 {
		t.Fatalf("Attach len=%d", len(got))
	}
	if got[0].Command != "" || !strings.HasPrefix(got[0].URL, "http://127.0.0.1:") || !strings.HasSuffix(got[0].URL, "/upstream/fixture") {
		t.Fatalf("stdio not rewritten: %+v", got[0])
	}
	if got[1].URL != "http://127.0.0.1:13705/mcp" || got[2].URL != "http://127.0.0.1:13715/mcp" {
		t.Fatalf("jevonsmcp rewritten: %+v %+v", got[1], got[2])
	}
}

func TestMCPHostStdioInitializeOverHTTP(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	got := h.Attach([]MCPServer{{Name: "fixture", Command: bin}})
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	req, err := http.NewRequestWithContext(wallclockguard.UntilTestTimeout(t), http.MethodPost, got[0].URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var msg struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Result.ServerInfo.Name != "mcpstdio-fixture" {
		t.Fatalf("serverInfo.name=%q", msg.Result.ServerInfo.Name)
	}
}

func TestMCPHostDoesNotStealDifferentRecipe(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	h := testMCPHost(t)
	first := h.Attach([]MCPServer{{Name: "fixture", Command: bin, Args: []string{"a"}}})
	second := h.Attach([]MCPServer{{Name: "fixture", Command: bin, Args: []string{"b"}}})
	if first[0].URL == "" || first[0].Command != "" {
		t.Fatalf("first not hosted: %+v", first[0])
	}
	if second[0].Command != bin || second[0].URL != "" {
		t.Fatalf("different recipe was stolen: %+v", second[0])
	}
	again := h.Attach([]MCPServer{{Name: "fixture", Command: bin, Args: []string{"a"}}})
	if again[0].URL != first[0].URL {
		t.Fatalf("same recipe did not reuse %q vs %q", again[0].URL, first[0].URL)
	}
}

// A changed HTTP origin must not leave a freshly minted seat on an old
// route, or silently return an unhosted server with no tools.
func TestMCPHostEnsureReplacesChangedHTTPOrigin(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("old"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fresh"))
	}))
	defer second.Close()
	h := testMCPHost(t)
	urlA, ok := h.ensure(MCPServer{Name: "fixture", Type: "http", URL: first.URL})
	if !ok || urlA == "" {
		t.Fatalf("first ensure = %q, %v", urlA, ok)
	}
	urlB, ok := h.ensure(MCPServer{Name: "fixture", Type: "http", URL: second.URL})
	if !ok || urlB != urlA {
		t.Fatalf("changed upstream ensure = %q, %v; want hosted %q", urlB, ok, urlA)
	}
	assertMCPHostResponse(t, urlB, "fresh")
	if got := h.upstreams.Get("fixture"); got != second.URL {
		t.Fatalf("remembered %q, want %q", got, second.URL)
	}
}

// A stale nested proxy remembered by an earlier host must not outrank the
// fresh direct URL presented by current configuration, even after the stale
// loopback was used to establish the in-memory route.
func TestMCPHostEnsureFreshDirectURLReplacesPersistedStaleLoopback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, mcpUpstreamsFile)
	stale := "http://127.0.0.1:52322/upstream/bullseye"
	b, err := json.Marshal(map[string]string{"bullseye": stale})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fresh upstream"))
	}))
	defer fresh.Close()
	h, err := NewMCPHost(&MCPHostArgs{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	// Simulate a previously minted seat whose loopback recipe was resolved
	// from the persisted stale store before config supplied the direct URL.
	if _, ok := h.ensure(MCPServer{Name: "bullseye", Type: "http", URL: stale}); !ok {
		t.Fatal("stale recipe did not register")
	}
	hosted, ok := h.ensure(MCPServer{Name: "bullseye", Type: "http", URL: fresh.URL})
	if !ok || hosted == "" {
		t.Fatalf("fresh ensure = %q, %v", hosted, ok)
	}
	assertMCPHostResponse(t, hosted, "fresh upstream")
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]string
	if err := json.Unmarshal(persisted, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["bullseye"] != fresh.URL {
		t.Fatalf("persisted %q, want %q", doc["bullseye"], fresh.URL)
	}
}

func assertMCPHostResponse(t *testing.T, endpoint, want string) {
	t.Helper()
	req, err := http.NewRequestWithContext(wallclockguard.UntilTestTimeout(t), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	if _, err := body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || body.String() != want {
		t.Fatalf("GET %s: status %d, body %q; want 200 %q", endpoint, resp.StatusCode, body.String(), want)
	}
}

func TestDirectModeDoesNotRewriteMCP(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	t.Setenv(broker.NoBrokerEnv, "1")
	t.Setenv(broker.SocketPathEnv, filepath.Join(t.TempDir(), "no.sock"))
	direct := &fakeAgentBackend{name: "fake-claude"}
	a, err := startConsideringBroker(Config{
		WorkDir:     t.TempDir(),
		SessionID:   "sid-direct",
		TermLogPath: "-",
		MCPServers:  []MCPServer{{Name: "fixture", Command: bin}},
	}, direct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	got := direct.request(t).Config.MCPServers
	if len(got) != 1 || got[0].Command != bin || got[0].URL != "" {
		t.Fatalf("direct mode rewrote MCP: %+v", got)
	}
}

// TestRegistryMCPHostSharesOneStdioProcessWithoutDaemon (🎯T75.5): two seats
// a direct-mode Registry starts, naming the same stdio recipe, both reach
// one hosted process; the persisted definitions keep the recipe, not the
// loopback URL, so a later host can attach them again.
func TestRegistryMCPHostSharesOneStdioProcessWithoutDaemon(t *testing.T) {
	bin := buildMCPStdioFixture(t)
	f := newSeatFixture(t)
	f.open(t, []AgentDef{
		{Name: "one", WorkDir: t.TempDir(), SessionID: "sid-1", MCPServers: []MCPServer{{Name: "fixture", Command: bin}}},
		{Name: "two", WorkDir: t.TempDir(), SessionID: "sid-2", MCPServers: []MCPServer{{Name: "fixture", Command: bin}}},
	})
	h := testMCPHost(t)
	f.reg.SetMCPHost(h)
	for _, name := range []string{"one", "two"} {
		if _, err := f.reg.Launch(name); err != nil {
			t.Fatal(err)
		}
	}
	urls := map[string]bool{}
	for _, name := range []string{"one", "two"} {
		f.mu.Lock()
		req := f.backends[name].request(t)
		f.mu.Unlock()
		got := req.Config.MCPServers
		if len(got) != 1 || got[0].Command != "" || !strings.HasSuffix(got[0].URL, "/upstream/fixture") {
			t.Fatalf("%s started with %+v, want the hosted URL", name, got)
		}
		urls[got[0].URL] = true
		if def := f.reg.Def(name); def.MCPServers[0].Command != bin || def.MCPServers[0].URL != "" {
			t.Fatalf("%s definition was rewritten: %+v", name, def.MCPServers)
		}
	}
	if len(urls) != 1 {
		t.Fatalf("seats reach different hosted URLs: %v", urls)
	}
	h.mu.Lock()
	n := len(h.stdio)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("hosted stdio backends = %d, want 1 shared", n)
	}
}

// TestMCPHostCarriesNoConsumerNames (🎯T75.5, 🎯T13): with no ConsumerOwned
// option the library hosts a server whatever it is called; leaving
// jevonsmcp on the caller's URL is the daemon's configuration.
func TestMCPHostCarriesNoConsumerNames(t *testing.T) {
	h, err := NewMCPHost(&MCPHostArgs{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	got := h.Attach([]MCPServer{{Name: "jevonsmcp", Type: "http", URL: "http://127.0.0.1:13705/mcp"}})
	if !strings.HasSuffix(got[0].URL, "/upstream/jevonsmcp") {
		t.Fatalf("library host special-cased a consumer name: %+v", got[0])
	}
	if _, err := NewMCPHost(&MCPHostArgs{}); err == nil {
		t.Fatal("a host without a state directory must be refused")
	}
}

// TestMCPHostSeedsEmptyStoresFromPreviousOwner: an empty token store is
// seeded from the first seed directory that has one.
func TestMCPHostSeedsEmptyStoresFromPreviousOwner(t *testing.T) {
	seed := t.TempDir()
	if err := os.WriteFile(filepath.Join(seed, mcpTokensFile), []byte(`{"atlassian":{"AccessToken":"tok"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := NewMCPHost(&MCPHostArgs{StateDir: t.TempDir(), SeedStateDirs: []string{filepath.Join(t.TempDir(), "absent"), seed}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if tok := h.tokens.Get("atlassian"); tok == nil || tok.AccessToken != "tok" {
		t.Fatalf("token store not seeded: %+v", tok)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯jevons/T612: a live switch must reload host tools from the restamped
// AgentDef.MCPServers list, not the launch-time snapshot.
func TestT612SyncSessionMCPServersUpdatesStartCfg(t *testing.T) {
	var applied []MCPServer
	a := &Agent{ops: agentOps{syncSessionMCPServers: func(_ *Agent, servers []MCPServer) {
		applied = slices.Clone(servers)
	}}}
	want := []MCPServer{{Name: "jevonsmcp", Type: "http", URL: "http://127.0.0.1:13705/mcp"}}
	a.SyncSessionMCPServers(want)
	if !reflect.DeepEqual(a.startCfg.MCPServers, want) {
		t.Fatalf("startCfg.MCPServers = %+v, want %+v", a.startCfg.MCPServers, want)
	}
	if !reflect.DeepEqual(applied, want) {
		t.Fatalf("sync hook saw %+v, want %+v", applied, want)
	}
}

func TestT612LiveSwitchReloadCarriesRestampedHostTools(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"jevons_agent_list","description":"list agents","inputSchema":{"type":"object"}}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"initialize"`) {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
			return
		}
		if strings.Contains(string(b), `"notifications/initialized"`) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, list)
	}))
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	fake := startFakeSidecar(t, nil)
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: Provider(omp.Anthropic), Name: "t612-seat", Model: "claude-opus", WorkDir: t.TempDir(), TermLogPath: "-",
		MCPServers: []MCPServer{{Name: "jevonsmcp", Type: "http", URL: dead.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	launch := fake.loaded()[0]
	var launchTools []hostJevonsTool
	if len(launch.Tools) > 0 {
		if err := json.Unmarshal(launch.Tools, &launchTools); err != nil {
			t.Fatal(err)
		}
	}
	if len(launchTools) != 0 {
		t.Fatalf("launch tools = %+v, want none from a dead jevonsmcp URL", launchTools)
	}

	live := srv.URL + "/mcp"
	agent.SyncSessionMCPServers([]MCPServer{{Name: "jevonsmcp", Type: "http", URL: live}})
	if err := agent.SwitchModel("", "claude-opus-5"); err != nil {
		t.Fatal(err)
	}
	loads := fake.loaded()
	if len(loads) != 2 {
		t.Fatalf("loads = %d, want launch + switch", len(loads))
	}
	var switchTools []hostJevonsTool
	if err := json.Unmarshal(loads[1].Tools, &switchTools); err != nil {
		t.Fatal(err)
	}
	if len(switchTools) != 1 || switchTools[0].Name != "jevons_agent_list" {
		t.Fatalf("switch tools = %+v, want jevons_agent_list from restamped jevonsmcp", switchTools)
	}
}

func TestT612RegistrySwitchSyncsMCPServersBeforeSidecarLoad(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"jevons_agent_list","description":"list","inputSchema":{"type":"object"}}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"initialize"`) {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
			return
		}
		if strings.Contains(string(b), `"notifications/initialized"`) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, list)
	}))
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	fake := startFakeSidecar(t, nil)
	reg, _ := switchRegistry(t)
	work := t.TempDir()
	if err := reg.Register(AgentDef{
		Name: "w", Provider: Provider(omp.Anthropic), Model: "claude-opus", WorkDir: work, SessionID: "s-1",
		MCPServers: []MCPServer{{Name: "jevonsmcp", Type: "http", URL: dead.URL}},
	}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("w")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Stop("w")
	if err := proc.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	def := reg.Def("w")
	def.MCPServers = []MCPServer{{Name: "jevonsmcp", Type: "http", URL: srv.URL + "/mcp"}}
	if err := reg.Register(*def); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SwitchModel(context.Background(), "w", ProviderCodex, "gpt-5.5", false); err != nil {
		t.Fatal(err)
	}
	loads := fake.loaded()
	if len(loads) != 2 {
		t.Fatalf("loads = %d", len(loads))
	}
	var tools []hostJevonsTool
	if err := json.Unmarshal(loads[1].Tools, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "jevons_agent_list" {
		t.Fatalf("switch load tools = %+v, want jevons_agent_list after registry sync", tools)
	}
}

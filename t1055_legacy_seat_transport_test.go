// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/tmuxagent"
	"github.com/marcelocantos/claudia/omp"
)

// TestT1055SessionDispatchIsSidecar drives the backend Start and Registry.Launch
// select. Fleet ids stay on the sidecar. The CLI fallback refuses.
func TestT1055SessionDispatchIsSidecar(t *testing.T) {
	ids := []Provider{
		"", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor,
		Provider(omp.Anthropic), Provider(omp.OpenAICodex), Provider(omp.XAIOAuth),
	}
	for _, id := range ids {
		backend := agentBackendFor(Config{Provider: id})
		if _, ok := backend.(ompAgentBackend); !ok {
			t.Errorf("agentBackendFor(%q) = %T, want omp", id, backend)
		}
	}
	for _, id := range []Provider{"", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor} {
		backend := agentBackendForProvider(id)
		got, err := backend.StartAgent(agentStartRequest{Context: context.Background(), Config: Config{Provider: id, WorkDir: t.TempDir()}})
		if got != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
			t.Errorf("fallback %q = (%v, %v), want legacy refusal", id, got, err)
		}
	}
}

// TestT1055RegistryAdoptClaudeConnectDoesNotUseCLI drives Registry.Adopt,
// the reattach path, for a claude row that still carries a CLI connect URL.
func TestT1055RegistryAdoptClaudeConnectDoesNotUseCLI(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	dir := t.TempDir()
	reg, err := NewRegistry(filepath.Join(dir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.StopAll)
	reg.SetDirect(true)
	if err := reg.Register(AgentDef{
		Name: "seat", Provider: ProviderClaude, SessionID: "sid-legacy",
		WorkDir: dir, ConnectURL: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = reg.Adopt("seat")
	if err == nil || !strings.Contains(err.Error(), "legacy CLI configuration") {
		t.Fatalf("adopt = %v, want the legacy CLI configuration refusal", err)
	}
}

// TestT1055CLIBackendsDoNotSpawn drives the concrete session backends.
// Product dispatch never selects them. A direct call must still refuse
// before any process exists.
func TestT1055CLIBackendsDoNotSpawn(t *testing.T) {
	backends := []struct {
		name string
		b    agentBackend
	}{
		{"claude", claudeAgentBackend{}},
		{"codex", codexAgentBackend{}},
		{"grok", grokAgentBackend{}},
		{"cursor", cursorAgentBackend{}},
		{"adopt", adoptClaudeBackend{}},
	}
	for _, tc := range backends {
		got, err := tc.b.StartAgent(agentStartRequest{
			Context:   context.Background(),
			SessionID: "sid",
			Config:    Config{WorkDir: t.TempDir()},
		})
		if got != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
			t.Errorf("%s StartAgent = (%v, %v), want legacy refusal", tc.name, got, err)
		}
	}
}

// TestT1055CursorOpeningWatchReportsStuck is the in-memory alarm for
// `watch := false` on the opening prompt. A client that can be woken and
// never is reports ErrCursorPromptStuck. The CLI seat tests that used to
// carry this alarm started a process.
func TestT1055CursorOpeningWatchReportsStuck(t *testing.T) {
	shortenCursorSilenceBound(t, 30*time.Millisecond)
	c := &cursorACPClient{
		sessionID: "s1",
		peerWoke:  make(chan struct{}, 1),
		stdin:     discardWrite{},
		onEvent:   func(Event) {},
	}
	err := c.Prompt("the opening brief")
	if !errors.Is(err, ErrCursorPromptStuck) {
		t.Fatalf("Prompt = %v, want ErrCursorPromptStuck", err)
	}
}

// TestT1055CompiledSpawnPathsDoNotExec drives the spawn entry points that
// remained after the concrete StartAgent bodies were removed. Each one
// returns the legacy refusal, and the marker binary is not executed.
func TestT1055CompiledSpawnPathsDoNotExec(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "t1055s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	t.Setenv(broker.NoBrokerEnv, "1")

	marker := filepath.Join(sockDir, "ran")
	bin := filepath.Join(sockDir, "bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", bin)
	work := t.TempDir()

	id, err := tmuxagent.SpawnWindow(work, "w", bin, nil)
	if id != "" || !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, err := Acquire(context.Background(), Config{WorkDir: work}); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := AcquireDirect(context.Background(), Config{WorkDir: work}); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	client, err := startCodexAppServer(bin, work, "", "", false, "", codexSandboxTuning{}, nil, nil, nil)
	if client != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		if client != nil {
			client.Close()
		}
		t.Fatalf("startCodexAppServer = (%v, %v), want legacy refusal", client, err)
	}
	if _, err := startGrokACP(bin, work, "", "", false, nil, nil, nil, nil); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("startGrokACP = %v, want legacy refusal", err)
	}
	if _, err := startGrokACPConnect(bin, work, "", "", false, nil, Config{}, nil, nil, nil); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("startGrokACPConnect = %v, want legacy refusal", err)
	}
	if _, err := spawnDetachedGrokServe(bin, "", nil); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("spawnDetachedGrokServe = %v, want legacy refusal", err)
	}
	if c, err := startCursorACP(t.Context(), bin, work, "", "", false, nil, nil, nil, nil); c != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("startCursorACP = (%v, %v), want legacy refusal", c, err)
	}

	reg, err := NewRegistry(filepath.Join(work, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name:      "ghost",
		WorkDir:   work,
		SessionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}); err != nil {
		t.Fatal(err)
	}
	reg.Stop("ghost")

	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("a spawn path executed the binary: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("a spawn path created a tmux socket: %v", statErr)
	}
}

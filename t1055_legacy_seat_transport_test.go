// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
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

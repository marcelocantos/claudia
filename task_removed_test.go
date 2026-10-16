// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

func TestTaskCLIRemovedBeforeBrokerOrProvider(t *testing.T) {
	for _, transport := range []string{"direct", "absent broker", "listening broker"} {
		t.Run(transport, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "task-refusal-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			sock := filepath.Join(dir, "broker.sock")
			t.Setenv(broker.SocketPathEnv, sock)
			t.Setenv(broker.NoBrokerEnv, "")
			var connections atomic.Int32
			if transport == "listening broker" {
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						c, err := ln.Accept()
						if err != nil {
							return
						}
						connections.Add(1)
						c.Close()
					}
				}()
				t.Cleanup(func() {
					ln.Close()
					<-done
					if connections.Load() != 0 {
						t.Errorf("removed Task contacted broker %d times", connections.Load())
					}
				})
			}
			// A resolver attempt would give a different error; no credentials are used.
			for _, key := range []string{"CLAUDE_BIN", "CODEX_BIN", "GROK_BIN", "CURSOR_BIN"} {
				t.Setenv(key, "/nonexistent/removed-task-cli")
			}
			for _, provider := range []Provider{"", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor, "anthropic", "openai-codex", "xai-oauth"} {
				t.Run(string(provider), func(t *testing.T) {
					cfg := TaskConfig{Provider: provider, WorkDir: t.TempDir(), RequireBroker: true, ToolPolicy: &TaskToolPolicy{MaxTurns: -1}}
					task := NewTask(cfg)
					task.SetDirect(transport == "direct")
					events, err := task.Run(t.Context(), "must not dispatch")
					if events != nil || !errors.Is(err, ErrTaskCLIRemoved) || task.Status() != TaskStatusError {
						t.Fatalf("Run = %v, %v, status=%s", events, err, task.Status())
					}
					events, err = RunBrokerTask(t.Context(), "must not dispatch", cfg)
					if events != nil || !errors.Is(err, ErrTaskCLIRemoved) {
						t.Fatalf("RunBrokerTask = %v, %v", events, err)
					}
				})
			}
		})
	}
}

func TestTaskCLIRefusalCodePreservesIdentity(t *testing.T) {
	err := brokerTaskError(&broker.ProtocolError{Code: broker.CodeTaskCLIRemoved, Msg: "removed by daemon"})
	if !errors.Is(err, ErrTaskCLIRemoved) {
		t.Fatalf("lost identity: %v", err)
	}
	for _, p := range []Provider{ProviderBedrock, ProviderOllama} {
		if err := TaskProviderError(p); err != nil {
			t.Fatalf("API backend %s refused: %v", p, err)
		}
		if !taskBackendForProvider(p).Capabilities().Task {
			t.Fatalf("API backend %s lost Task", p)
		}
	}
}

func TestTaskCLICatalogCannotSelectRemovedTransport(t *testing.T) {
	for _, row := range ModelCatalog() {
		if row.Task && TaskProviderError(row.Provider) != nil {
			t.Fatalf("unrunnable Task advertised: %+v", row)
		}
	}
	for _, purpose := range []ModelPurpose{"", ModelPurposeAnalysis} {
		_, err := Resolve(t.Context(), ModelPredicates{Mode: CapabilityTask, PreferPlan: true, Purpose: purpose,
			Usage: []PlanUsage{}, Intel: &ModelIntelArgs{Latest: []ModelObservation{}}})
		if err == nil {
			t.Fatal("selected a removed plan Task")
		}
	}
}

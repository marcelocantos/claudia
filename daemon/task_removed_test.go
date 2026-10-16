// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// Exercise the real socket entry point with an older client, bypassing SDK guards.
func TestTaskCLIRemovedAtDaemonBoundary(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	previous := daemonNewTask
	daemonNewTask = func(claudia.TaskConfig) *claudia.Task { t.Error("constructed a removed Task"); return nil }
	t.Cleanup(func() { daemonNewTask = previous })
	conn, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, provider := range []claudia.Provider{"", claudia.ProviderClaude, claudia.ProviderCodex, claudia.ProviderGrok, claudia.ProviderCursor, "anthropic", "openai-codex", "xai-oauth"} {
		for _, kind := range []broker.MessageType{broker.TypeTaskRun, broker.TypeTaskRunRestricted} {
			raw, err := claudia.EncodeTaskConfigWire(claudia.TaskConfig{Provider: provider, WorkDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			response := rawCall(t, conn, &broker.Request{ID: "removed", Type: kind, TaskRun: &broker.TaskRunRequest{Task: raw, Prompt: "must not dispatch", RequireBackgroundCapacity: true}})
			if response.Type != broker.TypeError || response.Error == nil || response.Error.Code != broker.CodeTaskCLIRemoved {
				t.Fatalf("%s %s: %+v", kind, provider, response)
			}
		}
	}
	if f.seatCount() != 0 {
		t.Fatal("Task refusal started a seat")
	}
}

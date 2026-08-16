// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

func startLibraryBroker(t *testing.T) *broker.Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	path, err := broker.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := broker.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := broker.Serve(&broker.ServeArgs{Listener: ln})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func TestUsingBrokerMirrorsDisabled(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	if usingBroker() {
		t.Error("usingBroker() true when CLAUDIA_NO_BROKER=1")
	}
	t.Setenv(broker.NoBrokerEnv, "0")
	if !usingBroker() {
		t.Error("usingBroker() false when CLAUDIA_NO_BROKER=0")
	}
	if err := os.Unsetenv(broker.NoBrokerEnv); err != nil {
		t.Fatal(err)
	}
	if !usingBroker() {
		t.Error("usingBroker() false when CLAUDIA_NO_BROKER is unset")
	}
}

func TestStartTakesBrokerlessFallbackWhenDisabled(t *testing.T) {
	srv := startLibraryBroker(t)
	t.Setenv(broker.NoBrokerEnv, "1")

	backend := &fakeAgentBackend{name: "fake-claude"}
	agent, err := startConsideringBroker(Config{
		WorkDir:     t.TempDir(),
		SessionID:   "no-broker",
		TermLogPath: "-",
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Stop)

	if n := srv.RequestCount(); n != 0 {
		t.Errorf("CLAUDIA_NO_BROKER=1 still reached the broker (%d requests)", n)
	}
	backend.mu.Lock()
	n := len(backend.requests)
	backend.mu.Unlock()
	if n != 1 {
		t.Fatalf("direct Start path was not taken: backend requests = %d", n)
	}
}

func TestStartProbesListeningBrokerWhenEnabled(t *testing.T) {
	srv := startLibraryBroker(t)
	t.Setenv(broker.NoBrokerEnv, "0")

	backend := &fakeAgentBackend{name: "fake-claude"}
	agent, err := startConsideringBroker(Config{
		WorkDir:     t.TempDir(),
		SessionID:   "broker-on",
		TermLogPath: "-",
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Stop)

	if n := srv.RequestCount(); n == 0 {
		t.Error("CLAUDIA_NO_BROKER=0 did not consult the listening broker")
	}
	backend.mu.Lock()
	n := len(backend.requests)
	backend.mu.Unlock()
	if n != 1 {
		t.Fatalf("direct fallback after the consult was not taken: backend requests = %d", n)
	}
}

func TestTaskRunTakesBrokerlessFallbackWhenDisabled(t *testing.T) {
	srv := startLibraryBroker(t)
	t.Setenv(broker.NoBrokerEnv, "1")

	backend := &fakeTaskBackend{
		name:   "fake-claude",
		events: []TaskEvent{{Type: TaskEventResult, Content: "ok"}},
	}
	task := newTaskWithBackend(TaskConfig{ID: "t", WorkDir: t.TempDir()}, backend)
	ch, err := task.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	if n := srv.RequestCount(); n != 0 {
		t.Errorf("CLAUDIA_NO_BROKER=1 Task.Run still reached the broker (%d requests)", n)
	}
	backend.mu.Lock()
	n := len(backend.requests)
	backend.mu.Unlock()
	if n != 1 {
		t.Fatalf("direct Task path was not taken: backend requests = %d", n)
	}
}

func TestTaskRunProbesListeningBrokerWhenEnabled(t *testing.T) {
	srv := startLibraryBroker(t)
	t.Setenv(broker.NoBrokerEnv, "0")

	backend := &fakeTaskBackend{
		name:   "fake-claude",
		events: []TaskEvent{{Type: TaskEventResult, Content: "ok"}},
	}
	task := newTaskWithBackend(TaskConfig{ID: "t", WorkDir: t.TempDir()}, backend)
	ch, err := task.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}

	if n := srv.RequestCount(); n == 0 {
		t.Error("CLAUDIA_NO_BROKER=0 Task.Run did not consult the listening broker")
	}
	backend.mu.Lock()
	n := len(backend.requests)
	backend.mu.Unlock()
	if n != 1 {
		t.Fatalf("direct fallback after the consult was not taken: backend requests = %d", n)
	}
}

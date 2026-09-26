// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestRunBrokerTaskDoesNotSpawnInProcess: with no daemon, RunBrokerTask
// returns ErrNoBroker and does not exec a provider. NewTask would.
func TestRunBrokerTaskDoesNotSpawnInProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", bin)
	t.Setenv(broker.NoBrokerEnv, "1")

	_, err := RunBrokerTask(context.Background(), "ping", TaskConfig{WorkDir: t.TempDir()})
	if !errors.Is(err, ErrNoBroker) {
		t.Fatalf("err = %v, want ErrNoBroker", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("RunBrokerTask spawned a provider in-process")
	}

	t.Setenv(broker.NoBrokerEnv, "")
	t.Setenv(broker.SocketPathEnv, filepath.Join(t.TempDir(), "missing.sock"))
	_, err = RunBrokerTask(context.Background(), "ping", TaskConfig{WorkDir: t.TempDir()})
	if !errors.Is(err, ErrNoBroker) {
		t.Fatalf("missing socket: %v, want ErrNoBroker", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("RunBrokerTask spawned a provider when the socket was absent")
	}
}

func TestPickByRemainingDoesNotSpawnWithoutBroker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", bin)
	t.Setenv(broker.NoBrokerEnv, "1")

	task := NewTask(TaskConfig{PickByRemaining: true, WorkDir: t.TempDir()})
	_, err := task.Run(context.Background(), "ping")
	if !errors.Is(err, ErrNoBroker) {
		t.Fatalf("err = %v, want ErrNoBroker", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("pick by remaining spawned a provider with no broker")
	}
}

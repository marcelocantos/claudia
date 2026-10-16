// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"errors"
	"os"
	"testing"
)

func TestTaskCLIRemovedWithoutAuthOrBinaryDiscovery(t *testing.T) {
	task := NewCodexTask(Config{ID: "old", SessionID: "history", Resolve: &ResolveArgs{
		Getenv:   func(string) string { t.Fatal("read provider environment"); return "" },
		LookPath: func(string) (string, error) { t.Fatal("resolved CLI"); return "", nil },
		Stat:     func(string) (os.FileInfo, error) { t.Fatal("read credentials or binary"); return nil, nil },
	}, RawLog: func([]byte) { t.Fatal("produced CLI output") }})
	ch, err := task.Run(t.Context(), "hello")
	if ch != nil || !errors.Is(err, ErrTaskCLIRemoved) || task.Status() != StatusError {
		t.Fatalf("Run=%v,%v status=%s", ch, err, task.Status())
	}
	if task.ID() != "old" || task.SessionID() != "history" {
		t.Fatal("compatibility metadata lost")
	}
	task.Stop()
	ch, err = task.Run(t.Context(), "again")
	if ch != nil || !errors.Is(err, ErrTaskCLIRemoved) || task.Status() != StatusStopped {
		t.Fatalf("stopped Run=%v,%v status=%s", ch, err, task.Status())
	}
}

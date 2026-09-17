// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

// Oracles for the exported seams the daemon package is built on (🎯T75.1).

// TestStartStubRunsTheStartPath: a stub goes through Start's own
// resolution, fail-closed resume, Goal loop and liveness, not a parallel one.
func TestStartStubRunsTheStartPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := StartStub(context.Background(), Config{WorkDir: t.TempDir(), SessionID: "sid-missing", RequireResume: true}, nil); err == nil ||
		!strings.Contains(err.Error(), "existing conversation required") {
		t.Fatalf("RequireResume without a transcript = %v, want Start's fail-closed refusal", err)
	}

	var mu sync.Mutex
	var sends []string
	var started StubStart
	var inFlight atomic.Bool
	exited := make(chan struct{})
	a, err := StartStub(context.Background(), Config{WorkDir: t.TempDir(), SessionID: "sid-stub", TermLogPath: "-", Goal: "ship it"}, &StubAgentOps{
		Send: func(text string) error {
			mu.Lock()
			sends = append(sends, text)
			mu.Unlock()
			inFlight.Store(true)
			return nil
		},
		PromptInFlight: inFlight.Load,
		Started:        func(s StubStart) { started = s },
		Exited:         exited,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if started.SessionID != "sid-stub" || !strings.HasSuffix(started.JSONLPath, "sid-stub.jsonl") || a.JSONLPath() != started.JSONLPath {
		t.Fatalf("started = %+v, agent transcript %q", started, a.JSONLPath())
	}
	if a.DaemonHeld() {
		t.Fatal("a stub is not daemon-held")
	}
	if err := a.Detach(); err == nil {
		t.Fatal("Detach must refuse an agent this process started")
	}
	if err := a.Send("go"); err != nil {
		t.Fatal(err)
	}
	inFlight.Store(false)
	a.PublishEvent(Event{Type: "assistant", Text: "partway", StopReason: "end_turn"})
	waitFor(t, "goal continuation", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sends) == 2 && strings.Contains(sends[1], "ship it")
	})
	close(exited)
	waitFor(t, "stub process exit", func() bool { return !a.Alive() })
}

// listenCounting is a Unix socket that counts connections and closes them.
func listenCounting(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	return path, &n
}

// TestRegistryLaunchersAndSetDirect: launchers replace the providers, and a
// direct Registry never dials a listening daemon while a default one does.
func TestRegistryLaunchersAndSetDirect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sock, dials := listenCounting(t)
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")

	var launched atomic.Int32
	launchers := &RegistryLaunchers{
		Start: func(ctx context.Context, cfg Config) (*Agent, error) {
			launched.Add(1)
			return StartStub(ctx, cfg, nil)
		},
	}
	for _, tc := range []struct {
		name      string
		direct    bool
		wantDials bool
	}{
		{"direct", true, false},
		{"default consults the daemon", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, launchedBefore := dials.Load(), launched.Load()
			reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
			if err != nil {
				t.Fatal(err)
			}
			reg.SetLaunchers(launchers)
			reg.SetDirect(tc.direct)
			if err := reg.Register(AgentDef{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid-" + tc.name, TermLogPath: "-"}); err != nil {
				t.Fatal(err)
			}
			a, err := reg.Launch("seat")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reg.StopAll)
			if launched.Load() != launchedBefore+1 || a.DaemonHeld() {
				t.Fatal("the launcher did not start the seat")
			}
			if dialed := dials.Load() > before; dialed != tc.wantDials {
				t.Fatalf("dialled the daemon socket = %v, want %v", dialed, tc.wantDials)
			}
		})
	}
}

// TestTaskSetDirectNeverDialsTheDaemon: a direct Task runs in this process
// with a daemon listening; the default Task asks the daemon first.
func TestTaskSetDirectNeverDialsTheDaemon(t *testing.T) {
	sock, dials := listenCounting(t)
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	for _, direct := range []bool{true, false} {
		before := dials.Load()
		task := newTaskWithBackend(TaskConfig{ID: "t", WorkDir: t.TempDir()}, &fakeTaskBackend{name: "fake-claude", events: []TaskEvent{{Type: TaskEventResult, Content: "ok"}}})
		task.SetDirect(direct)
		ch, err := task.Run(context.Background(), "go")
		if direct && err != nil {
			t.Fatal(err)
		}
		// The counting socket hangs up, which the default Task reports as
		// a broker failure rather than falling back: only the dial matters.
		if err == nil {
			for range ch {
			}
		}
		if dialed := dials.Load() > before; dialed == direct {
			t.Fatalf("direct=%v dialled the daemon socket = %v", direct, dialed)
		}
	}
}

// TestNewStubTaskRunsThroughTask: a stub task records session and result
// like a provider run, and hands the stub the consumer's raw-log func.
func TestNewStubTaskRunsThroughTask(t *testing.T) {
	var gotRaw bool
	task := NewStubTask(TaskConfig{ID: "stub"}, &StubTaskOps{
		Run: func(_ context.Context, run StubTaskRun) (<-chan TaskEvent, error) {
			gotRaw = run.RawLog != nil && run.Prompt == "go"
			ch := make(chan TaskEvent, 2)
			ch <- TaskEvent{Type: TaskEventInit, SessionID: "stub-sid"}
			ch <- TaskEvent{Type: TaskEventResult, Content: "done"}
			close(ch)
			return ch, nil
		},
	})
	task.SetRawLog(func([]byte) {})
	ch, err := task.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if !gotRaw || task.ClaudeID() != "stub-sid" || task.LastResult() != "done" {
		t.Fatalf("raw=%v session=%q result=%q", gotRaw, task.ClaudeID(), task.LastResult())
	}
}

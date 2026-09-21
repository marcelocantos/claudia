// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func lifecycleFixture(t *testing.T, start func(context.Context, Config) (*Agent, error)) *Registry {
	t.Helper()
	old := registryStart
	registryStart = start
	t.Cleanup(func() { registryStart = old })
	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"slow", "other"} {
		if err := r.Register(AgentDef{Name: name, SessionID: name, Provider: ProviderCursor}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func lifecycleAgent(cfg Config, stops *atomic.Int32) *Agent {
	ready := make(chan struct{})
	close(ready)
	a := &Agent{sessionID: cfg.SessionID, provider: cfg.Provider, alive: true, ready: ready}
	a.ops.stop = func(a *Agent) {
		if stops != nil {
			stops.Add(1)
		}
		a.mu.Lock()
		a.alive = false
		a.mu.Unlock()
	}
	return a
}

func lifecycleAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatal("lifecycle operation did not complete")
	}
	var zero T
	return zero
}

func TestRegistryLifecycleReadsAndOtherAgentWhileStarting(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r := lifecycleFixture(t, func(ctx context.Context, cfg Config) (*Agent, error) {
		if cfg.SessionID == "slow" {
			close(entered)
			<-release
		}
		return lifecycleAgent(cfg, nil), nil
	})
	done := make(chan error, 1)
	go func() { _, err := r.Launch("slow"); done <- err }()
	lifecycleAwait(t, entered)
	observed := make(chan error, 1)
	go func() {
		if len(r.List()) != 2 || r.Def("slow") == nil || r.Get("slow") != nil {
			observed <- errors.New("inconsistent startup view")
			return
		}
		_, err := r.Launch("other")
		observed <- err
	}()
	if err := lifecycleAwait(t, observed); err != nil {
		close(release)
		t.Fatal(err)
	}
	def := r.Def("slow")
	def.Description, def.Role, def.TargetID = "latest label", "auditor", "T59"
	if err := r.Register(*def); err != nil {
		close(release)
		t.Fatal(err)
	}
	def.Model = "conflicting-model"
	if err := r.Register(*def); !errors.Is(err, ErrLifecycleInProgress) {
		close(release)
		t.Fatalf("conflicting update=%v", err)
	}
	close(release)
	if err := lifecycleAwait(t, done); err != nil {
		t.Fatal(err)
	}
	if got := r.Def("slow"); got.Description != "latest label" || got.Role != "auditor" || got.TargetID != "T59" || got.Model != "" {
		t.Fatalf("publication overwrote metadata: %+v", got)
	}
}

func TestRegistryLifecycleConcurrentStartsShareOneProcess(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	r := lifecycleFixture(t, func(ctx context.Context, cfg Config) (*Agent, error) {
		starts.Add(1)
		close(entered)
		<-release
		return lifecycleAgent(cfg, nil), nil
	})
	const callers = 12
	done := make(chan *Agent, callers)
	errCh := make(chan error, callers)
	for range callers {
		go func() { a, err := r.Launch("slow"); errCh <- err; done <- a }()
	}
	lifecycleAwait(t, entered)
	close(release)
	first := lifecycleAwait(t, done)
	for range callers - 1 {
		if got := lifecycleAwait(t, done); got != first {
			t.Fatal("different process returned to concurrent caller")
		}
	}
	for range callers {
		if err := lifecycleAwait(t, errCh); err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("spawned %d processes", starts.Load())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lifecycle) != 0 {
		t.Fatal("lifecycle reservation leaked")
	}
}

func TestRegistryLifecycleStopJoinsCleanupAndFencesQueuedStart(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "remove"}[remove], func(t *testing.T) {
			entered, canceled, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var starts, stops atomic.Int32
			r := lifecycleFixture(t, func(ctx context.Context, cfg Config) (*Agent, error) {
				if starts.Add(1) == 1 {
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-cleanup
				}
				// A misbehaving backend can return a process after cancellation.
				return lifecycleAgent(cfg, &stops), nil
			})
			started := make(chan error, 1)
			go func() { _, err := r.Launch("slow"); started <- err }()
			lifecycleAwait(t, entered)
			queued := make(chan error, 1)
			go func() { _, err := r.Launch("slow"); queued <- err }()
			deadline := wallclockguard.UntilTestTimeout(t)
			// 🎯T97 exemption: a poll interval. A tick only re-reads state; the
			// wait's one failure is the UntilTestTimeout case, not this clock.
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
		waitQueued:
			for {
				select {
				case <-deadline.Done():
					t.Fatal("second start did not reserve its wait")
				case <-tick.C:
					r.mu.Lock()
					refs := r.lifecycle["slow"].refs
					r.mu.Unlock()
					if refs == 2 {
						break waitQueued
					}
				}
			}
			stopped := make(chan error, 1)
			go func() {
				if remove {
					stopped <- r.Remove("slow")
				} else {
					r.Stop("slow")
					stopped <- nil
				}
			}()
			lifecycleAwait(t, canceled)
			select {
			case <-stopped:
				t.Fatal("Stop returned before provider cleanup")
			default:
			}
			if err := r.Register(AgentDef{Name: "slow", SessionID: "replacement", Provider: ProviderCursor}); !errors.Is(err, ErrLifecycleInProgress) {
				t.Fatalf("registration raced old cleanup: %v", err)
			}
			if _, err := r.Launch("slow"); !errors.Is(err, ErrLifecycleInProgress) {
				t.Fatalf("start admitted during stop: %v", err)
			}
			close(cleanup)
			if err := lifecycleAwait(t, started); !errors.Is(err, context.Canceled) {
				t.Fatalf("late start=%v", err)
			}
			if err := lifecycleAwait(t, stopped); err != nil {
				t.Fatal(err)
			}
			if err := lifecycleAwait(t, queued); !errors.Is(err, ErrLifecycleInProgress) {
				t.Fatalf("queued start escaped stop: %v", err)
			}
			if r.Get("slow") != nil || stops.Load() != 1 || starts.Load() != 1 {
				t.Fatalf("late publication or cleanup: starts=%d stops=%d", starts.Load(), stops.Load())
			}
			if remove && r.Def("slow") != nil {
				t.Fatal("removed definition returned")
			}
			if err := r.Register(AgentDef{Name: "slow", SessionID: "replacement", Provider: ProviderCursor}); err != nil {
				t.Fatal(err)
			}
			if a, err := r.Launch("slow"); err != nil || a.SessionID() != "replacement" {
				t.Fatalf("new lifecycle failed: %v", err)
			}
		})
	}
}

func TestRegistryDefinitionSnapshotsOwnMCPData(t *testing.T) {
	r := lifecycleFixture(t, func(context.Context, Config) (*Agent, error) { t.Fatal("unexpected launch"); return nil, nil })
	def := *r.Def("slow")
	def.MCPServers = []MCPServer{{Name: "fixture", Args: []string{"original"}, Env: map[string]string{"KEY": "original"}, Headers: map[string]string{"X-Test": "original"}, Providers: []Provider{ProviderCursor}}}
	if err := r.Register(def); err != nil {
		t.Fatal(err)
	}
	def.MCPServers[0].Env["KEY"] = "mutated input"
	copy := r.Def("slow")
	copy.MCPServers[0].Args[0] = "mutated result"
	copy.MCPServers[0].Headers["X-Test"] = "mutated result"
	copy.MCPServers[0].Providers[0] = ProviderGrok
	for _, d := range r.List() {
		if d.Name == "slow" {
			d.MCPServers[0].Env["KEY"] = "mutated list"
		}
	}
	got := r.Def("slow").MCPServers[0]
	if got.Env["KEY"] != "original" || got.Args[0] != "original" || got.Headers["X-Test"] != "original" || got.Providers[0] != ProviderCursor {
		t.Fatalf("definition alias escaped: %+v", got)
	}
}

func TestRegistryCursorStopCancelsActualACPStartup(t *testing.T) {
	t.Setenv("CURSOR_BIN", writeFakeCursorACP(t))
	t.Setenv("FAKE_ACP_WITHHOLD", "session/load")
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("FAKE_ACP_REQUEST_LOG", logPath)
	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "saved-cursor-conversation"
	if err := r.Register(AgentDef{Name: "cursor", SessionID: sid, Provider: ProviderCursor, WorkDir: t.TempDir(), Materialized: true}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.Launch("cursor"); done <- err }()
	deadline := wallclockguard.UntilTestTimeout(t)
	// 🎯T97 exemption: a poll interval. A tick only re-reads state; the
	// wait's one failure is the UntilTestTimeout case, not this clock.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	pid := 0
waitLoad:
	for {
		select {
		case <-deadline.Done():
			t.Fatal("Cursor did not reach saved-session load")
		case <-tick.C:
			b, _ := os.ReadFile(logPath)
			for _, line := range strings.Split(string(b), "\n") {
				var row struct {
					PID    int
					Method string
				}
				if json.Unmarshal([]byte(line), &row) == nil && row.Method == "session/load" {
					pid = row.PID
					break waitLoad
				}
			}
		}
	}
	stopped := make(chan struct{})
	go func() { r.Stop("cursor"); close(stopped) }()
	lifecycleAwait(t, stopped)
	if err := lifecycleAwait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup error=%v", err)
	}
	if r.Get("cursor") != nil || r.ResumeDenied("cursor") != nil || r.Def("cursor").SessionID != sid {
		t.Fatal("stop published a handle, changed identity, or poisoned resume")
	}
	if processAlive(pid) {
		t.Fatalf("startup process %d survived Stop", pid)
	}
}

func TestRegistryAdoptionKeepsDistinctProcessEvidence(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })
	oldAdopt := registryAdopt
	registryAdopt = func(cfg Config) (*Agent, error) { return lifecycleAgent(cfg, nil), nil }
	t.Cleanup(func() { registryAdopt = oldAdopt })
	r := lifecycleFixture(t, func(context.Context, Config) (*Agent, error) {
		t.Fatal("adoption started a new process")
		return nil, nil
	})
	def := *r.Def("slow")
	def.Provider = ProviderClaude
	if err := r.Register(def); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdoptOrLaunch("slow"); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, `msg="agent adopted"`) || strings.Contains(got, `msg="agent started"`) {
		t.Fatalf("process provenance changed: %s", got)
	}
}

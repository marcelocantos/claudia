// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// Direct-mode oracles for 🎯T75.2 (ResumeAll) and 🎯T75.6 (seat events):
// a Registry with no daemon anywhere brings its seats back and reports what
// happened. The daemon suite covers the same behaviour through the tail.

type seatFixture struct {
	reg  *Registry
	path string

	mu       sync.Mutex
	backends map[string]*fakeAgentBackend
	events   []SeatEvent
}

func newSeatFixture(t *testing.T) *seatFixture {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	t.Setenv(broker.NoBrokerEnv, "1")
	f := &seatFixture{path: filepath.Join(t.TempDir(), "agents.json"), backends: map[string]*fakeAgentBackend{}}

	prevStart, prevAdopt := registryStart, registryAdopt
	t.Cleanup(func() { registryStart, registryAdopt = prevStart, prevAdopt })
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		b := &fakeAgentBackend{name: "fake-claude"}
		f.mu.Lock()
		f.backends[cfg.Name] = b
		f.mu.Unlock()
		return startWithBackendContext(ctx, cfg, b)
	}
	registryAdopt = func(Config) (*Agent, error) { return nil, ErrNoSessionWindow }
	return f
}

func (f *seatFixture) open(t *testing.T, defs []AgentDef) {
	t.Helper()
	writeAgentsFile(t, f.path, defs)
	reg, err := NewRegistry(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.reg = reg
	t.Cleanup(reg.StopAll)
	id := reg.SubscribeSeatEvents(func(ev SeatEvent) {
		f.mu.Lock()
		f.events = append(f.events, ev)
		f.mu.Unlock()
	})
	t.Cleanup(func() { reg.UnsubscribeSeatEvents(id) })
}

func writeAgentsFile(t *testing.T, path string, defs []AgentDef) {
	t.Helper()
	raw, err := json.Marshal(defs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *seatFixture) sends(name string) []string {
	f.mu.Lock()
	b := f.backends[name]
	f.mu.Unlock()
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sends...)
}

func (f *seatFixture) seatEvents(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, ev := range f.events {
		if ev.Name == name {
			out = append(out, string(ev.Kind)+"/"+string(ev.How))
		}
	}
	return out
}

// TestRegistryResumeAllWithoutDaemon: adopt what still runs without a
// nudge, launch and nudge what does not, report a seat that cannot start,
// and publish each outcome as a seat event in order.
func TestRegistryResumeAllWithoutDaemon(t *testing.T) {
	f := newSeatFixture(t)
	adoptable := &fakeAgentBackend{name: "fake-claude"}
	registryAdopt = func(cfg Config) (*Agent, error) {
		if cfg.Name == "still-there" {
			return startWithBackendContext(context.Background(), cfg, adoptable)
		}
		return nil, ErrNoSessionWindow
	}
	launch := registryStart
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		if cfg.Name == "broken" {
			return nil, os.ErrPermission
		}
		return launch(ctx, cfg)
	}
	f.open(t, []AgentDef{
		{Name: "still-there", WorkDir: t.TempDir(), SessionID: "sid-a", AutoStart: true},
		{Name: "gone", WorkDir: t.TempDir(), SessionID: "sid-b", AutoStart: true},
		{Name: "broken", WorkDir: t.TempDir(), SessionID: "sid-c", AutoStart: true},
		{Name: "idle", WorkDir: t.TempDir(), SessionID: "sid-d"},
	})

	out := f.reg.ResumeAll(context.Background(), &ResumeArgs{Nudge: "restart-nudge"})
	if len(out) != 3 {
		t.Fatalf("outcomes = %+v, want the three AutoStart seats", out)
	}
	byName := map[string]ResumeOutcome{}
	for _, o := range out {
		byName[o.Name] = o
	}
	if o := byName["still-there"]; o.How != ResumeAdopted || o.Nudged || o.Agent == nil {
		t.Fatalf("still-there = %+v, want adopted without nudge", o)
	}
	if o := byName["gone"]; o.How != ResumeLaunched || !o.Nudged || o.Agent == nil {
		t.Fatalf("gone = %+v, want launched and nudged", o)
	}
	if o := byName["broken"]; o.Err == nil || !strings.Contains(o.Err.Error(), "permission") {
		t.Fatalf("broken = %+v, want its start error", o)
	}
	adoptable.mu.Lock()
	adoptedSends := len(adoptable.sends)
	adoptable.mu.Unlock()
	if adoptedSends != 0 {
		t.Fatal("adopted seat was nudged")
	}
	if got := f.sends("gone"); len(got) != 1 || got[0] != "restart-nudge" {
		t.Fatalf("launched seat sends = %v", got)
	}
	if got := strings.Join(f.seatEvents("still-there"), ","); got != "resumed/adopted" {
		t.Fatalf("still-there events = %s", got)
	}
	if got := strings.Join(f.seatEvents("gone"), ","); got != "resumed/launched,nudged/" {
		t.Fatalf("gone events = %s", got)
	}
	if got := strings.Join(f.seatEvents("broken"), ","); got != "resume_failed/" {
		t.Fatalf("broken events = %s", got)
	}
	if f.seatEvents("idle") != nil {
		t.Fatal("a seat that is not AutoStart was resumed")
	}
}

// TestRegistryResumeAllNudgeDefaultAndDisabled: the empty Nudge sends
// DefaultRestartNudge stamped with Now; NoRestartNudge sends nothing.
func TestRegistryResumeAllNudgeDefaultAndDisabled(t *testing.T) {
	now := time.Date(2026, 9, 17, 8, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, nudge string
		want        []string
	}{
		{"default", "", []string{fmt.Sprintf(DefaultRestartNudge, now.Format(time.RFC3339))}},
		{"disabled", NoRestartNudge, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeatFixture(t)
			f.open(t, []AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid", AutoStart: true}})
			out := f.reg.ResumeAll(context.Background(), &ResumeArgs{Nudge: tc.nudge, Now: now})
			if len(out) != 1 || out[0].Err != nil {
				t.Fatalf("outcomes = %+v", out)
			}
			if got := f.sends("seat"); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("sends = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRegistryResumeAllConcurrencyIsBounded: with Concurrency 2 and four
// seats, no more than two provider starts are in flight at once.
func TestRegistryResumeAllConcurrencyIsBounded(t *testing.T) {
	f := newSeatFixture(t)
	var inflight, peak atomic.Int32
	release := make(chan struct{})
	launch := registryStart
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		return launch(ctx, cfg)
	}
	var defs []AgentDef
	for _, n := range []string{"a", "b", "c", "d"} {
		defs = append(defs, AgentDef{Name: n, WorkDir: t.TempDir(), SessionID: "sid-" + n, AutoStart: true})
	}
	f.open(t, defs)
	done := make(chan []ResumeOutcome)
	go func() {
		done <- f.reg.ResumeAll(context.Background(), &ResumeArgs{Nudge: NoRestartNudge, Concurrency: 2})
	}()
	waitFor(t, "two starts in flight", func() bool { return inflight.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := inflight.Load(); got != 2 {
		t.Fatalf("in flight = %d with bound 2", got)
	}
	close(release)
	for _, o := range <-done {
		if o.Err != nil {
			t.Fatalf("%s: %v", o.Name, o.Err)
		}
	}
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
}

// TestRegistryResumeAllRemintsRefusedConversation: a Cursor seat whose
// saved conversation the provider refuses to load is started on a fresh
// session, and the outcome names the session it left.
func TestRegistryResumeAllRemintsRefusedConversation(t *testing.T) {
	f := newSeatFixture(t)
	old := "b54f134f-f7ef-4780-a077-37132cd64d14"
	launch := registryStart
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		if cfg.RequireResume {
			return nil, fmt.Errorf("acp session/load %s: Invalid params (%w)", cfg.SessionID, ErrCursorResumeDenied)
		}
		return launch(ctx, cfg)
	}
	f.open(t, []AgentDef{{
		Name: "seat", WorkDir: t.TempDir(), SessionID: old,
		AutoStart: true, Materialized: true, Provider: ProviderCursor,
	}})
	out := f.reg.ResumeAll(context.Background(), &ResumeArgs{Nudge: NoRestartNudge})
	if len(out) != 1 || out[0].Err != nil || out[0].How != ResumeReminted || out[0].OldSessionID != old {
		t.Fatalf("outcome = %+v, want reminted off %s", out, old)
	}
	def := f.reg.Def("seat")
	if def == nil || def.SessionID == "" || def.SessionID == old {
		t.Fatalf("seat def = %+v, want a fresh session", def)
	}
	if got := strings.Join(f.seatEvents("seat"), ","); got != "resumed/reminted" {
		t.Fatalf("events = %s", got)
	}
}

// TestRegistryPublishesSeatGone: with a subscriber and no daemon, a
// launched seat whose process dies produces exactly one SeatGone on the
// next liveness tick; a live seat and a seat being stopped produce none.
func TestRegistryPublishesSeatGone(t *testing.T) {
	f := newSeatFixture(t)
	writeAgentsFile(t, f.path, []AgentDef{
		{Name: "dies", WorkDir: t.TempDir(), SessionID: "sid-1"},
		{Name: "lives", WorkDir: t.TempDir(), SessionID: "sid-2"},
	})
	reg, err := NewRegistry(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.StopAll)
	clock := broker.NewManualClock(time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC))
	reg.clock = clock
	var mu sync.Mutex
	var gone []string
	id := reg.SubscribeSeatEvents(func(ev SeatEvent) {
		if ev.Kind == SeatGone {
			mu.Lock()
			gone = append(gone, ev.Name)
			mu.Unlock()
		}
	})
	t.Cleanup(func() { reg.UnsubscribeSeatEvents(id) })

	dies, err := reg.Launch("dies")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Launch("lives"); err != nil {
		t.Fatal(err)
	}
	dies.mu.Lock()
	dies.alive = false
	dies.mu.Unlock()

	for range 3 {
		clock.Advance(seatWatchInterval)
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "gone event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(gone) > 0
	})
	mu.Lock()
	got := append([]string(nil), gone...)
	mu.Unlock()
	if strings.Join(got, ",") != "dies" {
		t.Fatalf("gone events = %v, want exactly one for the dead seat", got)
	}
}

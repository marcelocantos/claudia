// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// Hermetic fixtures for the daemon suite (🎯T2.9 / 🎯T2.10 / 🎯T2.11 / 🎯T3).
// The daemon runs in-process on a temp socket; its seats are claudia stubs
// started through claudia's own Start machinery, and the client side is the
// ordinary public API with the socket env pointed at it. Nothing here
// touches tmux, a provider binary, or a vendor usage endpoint. Everything
// used from claudia is exported API: this package builds only on that
// (🎯T75.1).

// TestMain keeps the suite off any daemon installed on the machine (a test
// that wants one starts its own and re-enables the consult), and off the
// developer's plan-usage cache, which a stub's rate-limit error marks stale.
func TestMain(m *testing.M) {
	if os.Getenv(broker.NoBrokerEnv) == "" {
		_ = os.Setenv(broker.NoBrokerEnv, "1")
	}
	cache, err := os.MkdirTemp("", "claudia-daemon-plan-cache")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CLAUDIA_PLAN_CACHE", cache)
	code := m.Run()
	_ = os.RemoveAll(cache)
	os.Exit(code)
}

// seatCaps is the turn contract every fixture seat reports: a Cursor-shaped
// seat that can steer at a breakpoint.
var seatCaps = claudia.TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: claudia.SteerBreakpoint, BusyOnSecondSubmit: claudia.BusySubmitSupersede}

// seat is one stub provider process the daemon started, with every verb it
// was asked for recorded.
type seat struct {
	inFlight atomic.Bool
	exited   chan struct{}
	exitOnce sync.Once

	mu         sync.Mutex
	started    claudia.StubStart
	sends      []string
	steers     []string
	interrupts int
	resizes    int
	stops      int
}

func newSeat() *seat { return &seat{exited: make(chan struct{})} }

func (s *seat) ops() *claudia.StubAgentOps {
	return &claudia.StubAgentOps{
		Send: func(text string) error {
			s.mu.Lock()
			s.sends = append(s.sends, text)
			s.mu.Unlock()
			s.inFlight.Store(true)
			return nil
		},
		Steer: func(text string) (claudia.DeliveryOutcome, error) {
			s.mu.Lock()
			s.steers = append(s.steers, text)
			s.mu.Unlock()
			return claudia.DeliveryOutcome{Mechanism: "fake_steer", SupersededTurnID: "t-1"}, nil
		},
		Interrupt: func() error {
			s.mu.Lock()
			s.interrupts++
			s.mu.Unlock()
			// A hard-stop closes the turn, as every real provider's does.
			s.inFlight.Store(false)
			return nil
		},
		Resize: func(uint16, uint16) error {
			s.mu.Lock()
			s.resizes++
			s.mu.Unlock()
			return nil
		},
		Stop: func() {
			s.mu.Lock()
			s.stops++
			s.mu.Unlock()
		},
		PromptInFlight: s.inFlight.Load,
		TurnCaps:       func() claudia.TurnCaps { return seatCaps },
		Started: func(st claudia.StubStart) {
			s.mu.Lock()
			s.started = st
			s.mu.Unlock()
		},
		Exited: s.exited,
	}
}

// exit ends the seat's process.
func (s *seat) exit() { s.exitOnce.Do(func() { close(s.exited) }) }

func (s *seat) start() claudia.StubStart {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *seat) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sends...)
}

func (s *seat) steered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.steers...)
}

func (s *seat) counts() (interrupts, resizes, stops int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interrupts, s.resizes, s.stops
}

// fixture is one daemon plus the stub seats it starts.
type fixture struct {
	d     *Daemon
	sock  string
	state string
	clock *broker.ManualClock

	// startHook and adoptHook, when set before boot, replace how seats are
	// started and adopted. startSeat is the default start.
	startHook func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error)
	adoptHook func(cfg claudia.Config) (*claudia.Agent, error)

	mu      sync.Mutex
	seats   []*seat
	fetches int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fixture{sock: filepath.Join(dir, "b.sock"), state: filepath.Join(dir, "state"),
		clock: broker.NewManualClock(time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC))}
	t.Setenv(broker.SocketPathEnv, f.sock)
	t.Setenv(broker.NoBrokerEnv, "")
	return f
}

// startSeat starts a stub seat and records it.
func (f *fixture) startSeat(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
	s := newSeat()
	f.mu.Lock()
	f.seats = append(f.seats, s)
	f.mu.Unlock()
	return claudia.StartStub(ctx, cfg, s.ops())
}

func (f *fixture) launchers() *claudia.RegistryLaunchers {
	return &claudia.RegistryLaunchers{
		Start: func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
			if f.startHook != nil {
				return f.startHook(ctx, cfg)
			}
			return f.startSeat(ctx, cfg)
		},
		Adopt: func(cfg claudia.Config) (*claudia.Agent, error) {
			if f.adoptHook != nil {
				return f.adoptHook(cfg)
			}
			return nil, claudia.ErrNoSessionWindow
		},
	}
}

// seat returns the i'th seat the daemon started, or nil.
func (f *fixture) seat(i int) *seat {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.seats) {
		return nil
	}
	return f.seats[i]
}

func (f *fixture) seatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seats)
}

func (f *fixture) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

// options is the fixture's daemon configuration; boot applies it.
func (f *fixture) options(usage []claudia.PlanUsage) Options {
	return Options{
		SocketPath:   f.sock,
		StateDir:     f.state,
		DisableIntel: true,
		RestartNudge: "restart-nudge",
		UsageFetch: func(context.Context) ([]claudia.PlanUsage, error) {
			f.mu.Lock()
			f.fetches++
			f.mu.Unlock()
			return usage, nil
		},
		clock:     f.clock,
		launchers: f.launchers(),
	}
}

// boot starts the daemon with no boot resume.
func (f *fixture) boot(t *testing.T, usage []claudia.PlanUsage) {
	t.Helper()
	opts := f.options(usage)
	opts.DisableResume = true
	f.bootWith(t, opts)
}

func (f *fixture) bootWith(t *testing.T, opts Options) {
	t.Helper()
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	f.d = d
	t.Cleanup(func() { _ = d.Close() })
}

// owner reports whether grant name currently has an owning connection.
func (f *fixture) owned(name string) bool {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	g := f.d.grants[name]
	return g != nil && g.owner != nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func collectEvents(a *claudia.Agent) func() []claudia.Event {
	var mu sync.Mutex
	var got []claudia.Event
	a.SubscribeEvents(func(ev claudia.Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	return func() []claudia.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]claudia.Event(nil), got...)
	}
}

// tailEvents subscribes a raw tail connection to the daemon and returns a
// snapshot function. It is subscribed before the observed action so the
// first event cannot race the subscription (the tailing ack is written
// after the subscription is live).
func tailEvents(t *testing.T, sock string) func() []broker.EventMessage {
	t.Helper()
	c, err := broker.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.WriteRequest(&broker.Request{ID: "tail", Type: broker.TypeTail, Tail: &broker.TailRequest{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := c.ReadResponse()
	if err != nil || ack.Type != broker.TypeTailing {
		t.Fatalf("tail ack = %v, %v", ack, err)
	}
	var mu sync.Mutex
	var got []broker.EventMessage
	go func() {
		for {
			resp, err := c.ReadResponse()
			if err != nil {
				return
			}
			if resp.Type == broker.TypeEvent {
				mu.Lock()
				got = append(got, *resp.Event)
				mu.Unlock()
			}
		}
	}()
	return func() []broker.EventMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]broker.EventMessage(nil), got...)
	}
}

func kinds(evs []broker.EventMessage, name string) []string {
	var out []string
	for _, ev := range evs {
		if name == "" || ev.Name == name {
			out = append(out, string(ev.Kind)+"/"+ev.Detail)
		}
	}
	return out
}

func writeGrantsTable(t *testing.T, state string, defs []claudia.AgentDef) {
	t.Helper()
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(defs)
	if err := os.WriteFile(filepath.Join(state, grantsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGrantsTable(t *testing.T, state string) map[string]claudia.AgentDef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, grantsFile))
	if err != nil {
		t.Fatal(err)
	}
	var defs []claudia.AgentDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatal(err)
	}
	out := map[string]claudia.AgentDef{}
	for _, d := range defs {
		out[d.Name] = d
	}
	return out
}

// rawSeat grants name over a raw wire connection and returns the
// connection, which owns the seat. Push messages (agent_event and friends)
// are id-less and interleave with answers, so callers use rawCall.
func rawSeat(t *testing.T, sock, name string) *broker.Conn {
	t.Helper()
	c, err := broker.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: name, WorkDir: t.TempDir(), SessionID: "sid-" + name, TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := rawCall(t, c, &broker.Request{ID: "grant", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: def}})
	if resp.Type != broker.TypeGranted {
		t.Fatalf("grant answered %s: %+v", resp.Type, resp.Error)
	}
	return c
}

// rawCall writes req and returns the answer carrying its id, skipping
// pushed messages.
func rawCall(t *testing.T, c *broker.Conn, req *broker.Request) *broker.Response {
	t.Helper()
	if err := c.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatalf("waiting for %s answer: %v", req.ID, err)
		}
		if resp.ID == req.ID {
			return resp
		}
	}
}

// transcriptLines is a synthetic Claude transcript with three genuine user
// turns (ALPHA, turn2, CHARLIE); the tool_result between turn2 and turn3 is
// recorded with role "user" but is not a turn.
var transcriptLines = []string{
	`{"type":"summary","summary":"prior session"}`,
	`{"type":"user","message":{"role":"user","content":"turn1 ALPHA"}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}`,
	`{"type":"user","message":{"role":"user","content":"turn2 run a tool"}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash"}],"stop_reason":"tool_use"}}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"finished"}],"stop_reason":"end_turn"}}`,
	`{"type":"user","message":{"role":"user","content":"turn3 CHARLIE"}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}`,
}

func writeTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(transcriptLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildMCPStdioFixture builds claudia's stdio MCP test server.
func buildMCPStdioFixture(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mcpstdio")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/mcpstdio")
	cmd.Dir = ".." // the module root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build mcpstdio fixture: %v\n%s", err, out)
	}
	return bin
}

// startLiveDaemon runs a daemon with real provider seats in this process on
// a temp socket and points the consumer API at it, so a live test exercises
// the daemon path without touching an installed daemon.
//
// Seats get their own tmux server too. On the shared claudia socket a host's
// Jevons pane census reaps any window it has no registry entry for, which
// killed test seats mid-turn every three minutes (2026-09-17).
func startLiveDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	tmuxSock := filepath.Join(dir, "tmux.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", tmuxSock)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", tmuxSock, "kill-server").Run() })
	sock := filepath.Join(dir, "b.sock")
	d, err := New(Options{
		SocketPath: sock, StateDir: filepath.Join(dir, "state"),
		DisableResume: true, DisableIntel: true, DisableMCPHost: true,
		UsageFetch: func(context.Context) ([]claudia.PlanUsage, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	return d
}

// termLogTail returns the last few KB of a terminal log.
func termLogTail(path string) string {
	if path == "" {
		return "(no terminal log)"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	if len(raw) > 4000 {
		raw = raw[len(raw)-4000:]
	}
	return string(raw)
}

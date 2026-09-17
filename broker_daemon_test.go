// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// Hermetic oracles for the daemon (🎯T2.9 / 🎯T2.10 / 🎯T2.11 / 🎯T3). The
// daemon runs in-process on a temp socket with its direct starter pointed at
// fakeAgentBackend, and the client side is the ordinary public API with the
// socket env pointed at it. Nothing here touches tmux, a provider binary, or
// a vendor usage endpoint.

// daemonFixture is one daemon plus the fake backends it starts.
type daemonFixture struct {
	d     *BrokerDaemon
	sock  string
	state string
	clock *broker.ManualClock

	mu       sync.Mutex
	backends []*fakeAgentBackend
	modes    []*modeBackend
	fetches  int
}

// modeBackend is fakeAgentBackend plus the 🎯T72.2 turn hooks, so the
// daemon-side Agent can steer and a test can see which verb ran (🎯T72.3).
// Its caps are a Cursor-shaped contract; the fake underneath still counts
// sends and interrupts.
type modeBackend struct {
	*fakeAgentBackend
	mu     sync.Mutex
	steers []string
}

// modeBackendCaps is what a modeBackend seat reports.
var modeBackendCaps = TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: SteerBreakpoint, BusyOnSecondSubmit: BusySubmitSupersede}

func (b *modeBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	start, err := b.fakeAgentBackend.StartAgent(req)
	if err != nil {
		return nil, err
	}
	start.Ops.steer = func(_ *Agent, text string) (DeliveryOutcome, error) {
		b.mu.Lock()
		b.steers = append(b.steers, text)
		b.mu.Unlock()
		return DeliveryOutcome{Mechanism: "fake_steer", SupersededTurnID: "t-1"}, nil
	}
	start.Ops.turnCaps = func(*Agent) TurnCaps { return modeBackendCaps }
	return start, nil
}

func (b *modeBackend) steered() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.steers...)
}

func (f *daemonFixture) backend(i int) *fakeAgentBackend {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.backends) {
		return nil
	}
	return f.backends[i]
}

func (f *daemonFixture) mode(i int) *modeBackend {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.modes) {
		return nil
	}
	return f.modes[i]
}

func (f *daemonFixture) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

// startDaemon boots a daemon whose seats are fakeAgentBackends. resume
// controls whether it brings back seats persisted in state.
func startDaemon(t *testing.T, resume bool, usage []PlanUsage) *daemonFixture {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &daemonFixture{sock: filepath.Join(dir, "b.sock"), state: filepath.Join(dir, "state"),
		clock: broker.NewManualClock(time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC))}

	prevStart, prevAdopt := registryStartDirect, registryAdopt
	registryStartDirect = func(ctx context.Context, cfg Config) (*Agent, error) {
		b := &fakeAgentBackend{name: "fake-claude"}
		m := &modeBackend{fakeAgentBackend: b}
		f.mu.Lock()
		f.backends = append(f.backends, b)
		f.modes = append(f.modes, m)
		f.mu.Unlock()
		return startWithBackendContext(ctx, cfg, m)
	}
	registryAdopt = func(cfg Config) (*Agent, error) { return nil, ErrNoSessionWindow }
	t.Cleanup(func() { registryStartDirect, registryAdopt = prevStart, prevAdopt })

	t.Setenv(broker.SocketPathEnv, f.sock)
	t.Setenv(broker.NoBrokerEnv, "")
	return f
}

func (f *daemonFixture) boot(t *testing.T, resume bool, usage []PlanUsage) {
	t.Helper()
	d, err := NewBrokerDaemon(BrokerDaemonOptions{
		SocketPath:    f.sock,
		StateDir:      f.state,
		DisableResume: !resume,
		DisableIntel:  true,
		RestartNudge:  "restart-nudge",
		UsageFetch: func(context.Context) ([]PlanUsage, error) {
			f.mu.Lock()
			f.fetches++
			f.mu.Unlock()
			return usage, nil
		},
		clock: f.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.d = d
	t.Cleanup(func() { _ = d.Close() })
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

func collectEvents(a *Agent) func() []Event {
	var mu sync.Mutex
	var got []Event
	a.SubscribeEvents(func(ev Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	return func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), got...)
	}
}

// TestBrokerDaemonGrantIsDifferentialWithDirectStart is 🎯T2.10's
// acceptance: the same fake, once through the daemon and once direct,
// yields the same Event sequence on the consumer's handle, and Send /
// Interrupt / Resize reach the provider.
func TestBrokerDaemonGrantIsDifferentialWithDirectStart(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)

	turn := []Event{
		{Type: "assistant", Text: "hello", Model: "claude-opus-5", Usage: Usage{InputTokens: 3, OutputTokens: 5}},
		{Type: "progress", ProgressType: "tool_use", ToolCallID: "t1", ToolTitle: "Read"},
		{Type: "assistant", Text: "done", StopReason: "end_turn"},
	}

	// Through the daemon.
	a, err := Start(Config{Name: "seat-1", WorkDir: t.TempDir(), SessionID: "sid-1", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start via daemon: %v", err)
	}
	t.Cleanup(a.Stop)
	if a.brokerGrant != "seat-1" {
		t.Fatalf("handle is not broker-held: grant=%q", a.brokerGrant)
	}
	daemonSide := f.backend(0)
	if daemonSide == nil {
		t.Fatal("daemon started no provider backend")
	}
	req := daemonSide.request(t)
	if req.Config.Name != "seat-1" || req.SessionID != "sid-1" {
		t.Fatalf("daemon-side start request = %+v", req.Config)
	}
	brokered := collectEvents(a)
	if err := a.Send("hi"); err != nil {
		t.Fatal(err)
	}
	if err := a.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	daemonSide.mu.Lock()
	sends, interrupts, resizes := daemonSide.sends, daemonSide.interrupts, len(daemonSide.resizes)
	daemonSide.mu.Unlock()
	if len(sends) != 1 || sends[0] != "hi" || interrupts != 1 || resizes != 1 {
		t.Fatalf("provider saw sends=%v interrupts=%d resizes=%d", sends, interrupts, resizes)
	}
	proc := f.d.reg.Get("seat-1")
	if proc == nil {
		t.Fatal("daemon holds no proc for seat-1")
	}
	go func() {
		for _, ev := range turn {
			proc.PublishEvent(ev)
		}
	}()
	text, err := a.WaitForResponse(context.Background())
	if err != nil || text != "hello\ndone" {
		t.Fatalf("WaitForResponse via daemon = %q, %v", text, err)
	}
	if u := a.Usage(); u.InputTokens != 3 || u.OutputTokens != 5 {
		t.Fatalf("usage not accumulated on the handle: %+v", u)
	}
	if a.Model() != "claude-opus-5" {
		t.Fatalf("model not tracked on the handle: %q", a.Model())
	}

	// Direct, same fake.
	direct := &fakeAgentBackend{name: "fake-claude"}
	t.Setenv(broker.NoBrokerEnv, "1")
	b, err := startConsideringBroker(Config{WorkDir: t.TempDir(), SessionID: "sid-2", TermLogPath: "-"}, direct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	directEvents := collectEvents(b)
	for _, ev := range turn {
		b.PublishEvent(ev)
	}

	waitFor(t, "brokered events", func() bool { return len(brokered()) >= len(turn) })
	got, want := brokered(), directEvents()
	if len(got) != len(want) {
		t.Fatalf("event count via daemon %d, direct %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		g.SessionID, w.SessionID = "", ""
		if !reflect.DeepEqual(g, w) {
			t.Errorf("event %d differs\n daemon: %+v\n direct: %+v", i, g, w)
		}
	}
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
	def, err := encodeGrantDefWire(configToGrantDef(name, Config{WorkDir: t.TempDir(), SessionID: "sid-" + name, TermLogPath: "-"}, nil))
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

// TestBrokerDaemonSendModeDispatch is 🎯T72.3's daemon clause, over the raw
// wire so the sent response is read as bytes rather than through the
// handle: each mode reaches the verb the design maps it to, and the
// response names the mechanism that ran. The fake flips itself busy on
// its first send, so phase_before moves from idle to in_turn as it would
// on a real seat.
func TestBrokerDaemonSendModeDispatch(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	c := rawSeat(t, f.sock, "modes")
	seat, mode := f.backend(0), f.mode(0)
	if seat == nil {
		t.Fatal("daemon started no provider backend")
	}
	counts := func() (sends []string, interrupts int) {
		seat.mu.Lock()
		defer seat.mu.Unlock()
		return append([]string(nil), seat.sends...), seat.interrupts
	}
	send := func(id, text string, m broker.SendMode) *broker.SentResponse {
		t.Helper()
		resp := rawCall(t, c, &broker.Request{ID: id, Type: broker.TypeSend, Send: &broker.SendRequest{Name: "modes", Text: text, Mode: m}})
		if resp.Type != broker.TypeSent {
			t.Fatalf("%s answered %s: %+v", id, resp.Type, resp.Error)
		}
		return resp.Sent
	}
	expect := func(what string, got, want *broker.SentResponse) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: sent = %+v, want %+v", what, got, want)
		}
	}

	// Steer on an idle seat folds to a submit and says so.
	expect("steer idle", send("st0", "first", broker.SendModeSteer),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeSteer, Mechanism: MechanismSubmit, PhaseBefore: string(TurnIdle)})
	// Absent mode is submit: today's client keeps today's behaviour.
	expect("submit", send("s1", "one", ""),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeSubmit, Mechanism: MechanismSubmit, PhaseBefore: string(TurnInTurn)})
	// Steer with a turn open runs the provider's steer, not Send.
	expect("steer", send("st1", "also", broker.SendModeSteer),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeSteer, Mechanism: "fake_steer", PhaseBefore: string(TurnInTurn), SupersededTurnID: "t-1"})
	sends, interrupts := counts()
	if !reflect.DeepEqual(sends, []string{"first", "one"}) || interrupts != 0 || !reflect.DeepEqual(mode.steered(), []string{"also"}) {
		t.Fatalf("after steer: sends=%v interrupts=%d steers=%v", sends, interrupts, mode.steered())
	}
	// Interrupt with a turn open hard-stops, then submits.
	expect("interrupt", send("i1", "stop", broker.SendModeInterrupt),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeInterrupt, Mechanism: MechanismInterruptThenSubmit, PhaseBefore: string(TurnInTurn)})
	sends, interrupts = counts()
	if !reflect.DeepEqual(sends, []string{"first", "one", "stop"}) || interrupts != 1 {
		t.Fatalf("after interrupt: sends=%v interrupts=%d", sends, interrupts)
	}
	// Queue is ack-only: the seat never sees the text.
	expect("queue", send("q1", "later", broker.SendModeQueue),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeQueue, Mechanism: MechanismClientQueue, PhaseBefore: string(TurnInTurn)})
	// The bare interrupt request is untouched by send.mode.
	resp := rawCall(t, c, &broker.Request{ID: "i2", Type: broker.TypeInterrupt, Interrupt: &broker.NamedRequest{Name: "modes"}})
	if resp.Type != broker.TypeInterrupted || resp.Interrupted.Name != "modes" {
		t.Fatalf("interrupt answered %s: %+v", resp.Type, resp.Error)
	}
	// An unknown mode is refused at the wire, never reaching the seat.
	resp = rawCall(t, c, &broker.Request{ID: "x1", Type: broker.TypeSend, Send: &broker.SendRequest{Name: "modes", Text: "?", Mode: "nudge"}})
	if resp.Type != broker.TypeError || resp.Error.Code != broker.CodeUnsupportedValue || resp.Error.Field != "mode" {
		t.Fatalf("bad mode answered %s %+v", resp.Type, resp.Error)
	}
	sends, interrupts = counts()
	if len(sends) != 3 || interrupts != 2 || len(mode.steered()) != 1 {
		t.Errorf("seat saw sends=%v interrupts=%d steers=%v after queue, interrupt and a refused mode", sends, interrupts, mode.steered())
	}

	// turn_caps rides agent_info, and the grants snapshot, from the seat.
	want := &broker.TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: string(SteerBreakpoint), BusyOnSecondSubmit: BusySubmitSupersede}
	info := rawCall(t, c, &broker.Request{ID: "ai", Type: broker.TypeAgentInfo, AgentInfo: &broker.NamedRequest{Name: "modes"}})
	if info.AgentInfo == nil || !reflect.DeepEqual(info.AgentInfo.TurnCaps, want) {
		t.Errorf("agent_info turn_caps = %+v, want %+v", info.AgentInfo, want)
	}
	if got := f.d.grantList(); len(got) != 1 || !reflect.DeepEqual(got[0].TurnCaps, want) {
		t.Errorf("grants turn_caps = %+v, want %+v", got, want)
	}
}

// TestBrokerHandleForwardsSteerAndTurnCaps is the client half of 🎯T72.3:
// a broker-held handle's Steer, SendMode and TurnCaps reach the daemon's
// seat through send.mode and turn_caps, with the daemon's outcome on the
// handle, rather than answering from the provider contract alone.
func TestBrokerHandleForwardsSteerAndTurnCaps(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	a, err := Start(Config{Name: "seat-m", WorkDir: t.TempDir(), SessionID: "sid-m", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	seat, mode := f.backend(0), f.mode(0)

	if caps := a.TurnCaps(); caps != modeBackendCaps {
		t.Fatalf("TurnCaps via daemon = %+v, want %+v", caps, modeBackendCaps)
	}
	if got := f.d.reg.Get("seat-m").TurnPhase(); got != TurnIdle {
		t.Fatalf("seat phase = %s before any send", got)
	}
	if _, err := a.Steer("early"); !errors.Is(err, ErrTurnIdle) {
		t.Fatalf("Steer on an idle seat: err = %v, want ErrTurnIdle", err)
	}
	if err := a.Send("go"); err != nil {
		t.Fatal(err)
	}
	out, err := a.Steer("and this")
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if out.Mechanism != "fake_steer" || out.SupersededTurnID != "t-1" || out.PhaseBefore != TurnInTurn || out.Mode != DeliverySteer {
		t.Fatalf("Steer outcome = %+v", out)
	}
	if !reflect.DeepEqual(mode.steered(), []string{"and this"}) {
		t.Fatalf("seat steers = %v", mode.steered())
	}
	out, err = a.SendMode("now", DeliveryInterrupt)
	if err != nil {
		t.Fatalf("SendMode interrupt: %v", err)
	}
	if out.Mechanism != MechanismInterruptThenSubmit {
		t.Fatalf("interrupt outcome = %+v", out)
	}
	seat.mu.Lock()
	sends, interrupts := append([]string(nil), seat.sends...), seat.interrupts
	seat.mu.Unlock()
	if !reflect.DeepEqual(sends, []string{"go", "now"}) || interrupts != 1 {
		t.Fatalf("seat saw sends=%v interrupts=%d", sends, interrupts)
	}
}

// TestDeliverSendOnStubAgent is the dispatch table on a stub whose every
// verb is observable (🎯T72.3): each wire mode runs exactly the Agent verb
// the design maps it to, the sent response carries the mechanism the verb
// reported, and turn_caps is the stub's own answer. The daemon suite above
// proves the same over the socket; this pins the table without one.
func TestDeliverSendOnStubAgent(t *testing.T) {
	var ran []string
	phase := TurnInTurn
	stub := NewStubAgentOps(&StubAgentOps{
		Provider: ProviderCursor,
		Send:     func(text string) error { ran = append(ran, "send:"+text); return nil },
		Steer: func(text string) (DeliveryOutcome, error) {
			ran = append(ran, "steer:"+text)
			return DeliveryOutcome{Mechanism: "stub_steer", SupersededTurnID: "t-2"}, nil
		},
		Interrupt: func() error { ran = append(ran, "interrupt"); phase = TurnIdle; return nil },
		TurnPhase: func() TurnPhase { return phase },
		TurnCaps:  func() TurnCaps { return modeBackendCaps },
	})
	cases := []struct {
		mode broker.SendMode
		text string
		want *broker.SentResponse
		ran  []string
	}{
		{broker.SendModeSteer, "fold", &broker.SentResponse{Mode: broker.SendModeSteer, Mechanism: "stub_steer", PhaseBefore: "in_turn", SupersededTurnID: "t-2"}, []string{"steer:fold"}},
		{broker.SendModeQueue, "hold", &broker.SentResponse{Mode: broker.SendModeQueue, Mechanism: MechanismClientQueue, PhaseBefore: "in_turn"}, nil},
		{broker.SendModeInterrupt, "stop", &broker.SentResponse{Mode: broker.SendModeInterrupt, Mechanism: MechanismInterruptThenSubmit, PhaseBefore: "in_turn"}, []string{"interrupt", "send:stop"}},
		{broker.SendModeSubmit, "go", &broker.SentResponse{Mode: broker.SendModeSubmit, Mechanism: MechanismSubmit, PhaseBefore: "idle"}, []string{"send:go"}},
	}
	for _, tc := range cases {
		ran = nil
		got, err := deliverSend(stub, &broker.SendRequest{Name: "stub", Text: tc.text, Mode: tc.mode})
		if err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: sent = %+v, want %+v", tc.mode, got, tc.want)
		}
		if !reflect.DeepEqual(ran, tc.ran) {
			t.Errorf("%s: verbs ran = %v, want %v", tc.mode, ran, tc.ran)
		}
	}
	if _, err := deliverSend(stub, &broker.SendRequest{Name: "stub", Text: "?", Mode: "nudge"}); err == nil {
		t.Error("unknown mode was dispatched")
	}
	want := &broker.TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: string(SteerBreakpoint), BusyOnSecondSubmit: BusySubmitSupersede}
	if got := turnCapsWire(stub); !reflect.DeepEqual(got, want) {
		t.Errorf("turn_caps = %+v, want %+v", got, want)
	}
}

// TestBrokerTurnCapsMirrorIsComplete keeps broker.TurnCaps field for field
// with TurnCaps, the way the other wire mirrors are held (🎯T24: a field
// added to one side without the other would vanish on the socket).
func TestBrokerTurnCapsMirrorIsComplete(t *testing.T) {
	wire := map[string]bool{}
	for f := range reflect.TypeFor[broker.TurnCaps]().Fields() {
		wire[f.Name] = true
	}
	for f := range reflect.TypeFor[TurnCaps]().Fields() {
		if !wire[f.Name] {
			t.Errorf("TurnCaps.%s has no slot on broker.TurnCaps", f.Name)
		}
		delete(wire, f.Name)
	}
	for name := range wire {
		t.Errorf("broker.TurnCaps.%s is not a TurnCaps field", name)
	}
}

// TestBrokerDaemonReclaimAfterConsumerRestart is 🎯T2.11: the consumer's
// connection dies without a release, the seat keeps running, what it says
// meanwhile is retained, and a new consumer reclaims it by name with that
// history replayed ahead of live traffic.
func TestBrokerDaemonReclaimAfterConsumerRestart(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)

	first, err := Start(Config{Name: "seat-r", WorkDir: t.TempDir(), SessionID: "sid-r", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	proc := f.d.reg.Get("seat-r")
	// Simulate the consumer dying: drop the socket without releasing.
	first.mcpCleanup()
	waitFor(t, "seat detached", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants["seat-r"]
		return g != nil && g.owner == nil
	})
	if !proc.Alive() {
		t.Fatal("seat was stopped when the consumer went away")
	}
	waitFor(t, "handle marked unreachable", func() bool { return !first.Alive() })

	proc.PublishEvent(Event{Type: "assistant", Text: "while you were away", StopReason: "end_turn"})
	proc.PublishEvent(Event{Type: "progress", ProgressType: "tool_use", ToolTitle: "Bash"})

	second, err := Start(Config{Name: "seat-r", WorkDir: t.TempDir(), SessionID: "sid-r", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	t.Cleanup(second.Stop)
	if second.SessionID() != "sid-r" {
		t.Fatalf("reclaimed a different session: %s", second.SessionID())
	}
	if f.backend(1) != nil {
		t.Fatal("reclaim started a second provider process")
	}
	got := collectEvents(second)
	proc.PublishEvent(Event{Type: "assistant", Text: "live", StopReason: "end_turn"})
	waitFor(t, "replay + live", func() bool { return len(got()) >= 3 })
	evs := got()
	if evs[0].Text != "while you were away" || evs[1].ToolTitle != "Bash" || evs[2].Text != "live" {
		t.Fatalf("replay order wrong: %+v", evs)
	}

	// A third consumer cannot steal a held seat.
	_, err = Start(Config{Name: "seat-r", WorkDir: t.TempDir(), SessionID: "sid-r", TermLogPath: "-"})
	var pe *broker.ProtocolError
	if !errors.As(err, &pe) || pe.Code != broker.CodeGrantHeld {
		t.Fatalf("second owner: err = %v, want grant_held", err)
	}
}

// TestBrokerDaemonStopReleasesTheSeat: Stop on the handle tears the seat
// down on the daemon and forgets it.
func TestBrokerDaemonStopReleasesTheSeat(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	a, err := Start(Config{Name: "seat-s", WorkDir: t.TempDir(), SessionID: "sid-s", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	a.Stop()
	waitFor(t, "seat removed", func() bool { return f.d.reg.Def("seat-s") == nil })
	be := f.backend(0)
	be.mu.Lock()
	stops := be.stops
	be.mu.Unlock()
	if stops != 1 {
		t.Fatalf("provider stops = %d", stops)
	}
}

// TestBrokerDaemonRegistryLaunchGoesThroughDaemon: a consumer Registry
// (Jevons's surface) launches by name and the daemon owns the process.
func TestBrokerDaemonRegistryLaunchGoesThroughDaemon(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	def := AgentDef{Name: "jv-worker", WorkDir: t.TempDir(), SessionID: "sid-jv", Purpose: PurposeWork, Parent: "jevons-po",
		PermissionMode: "plan", ExtraArgs: []string{"--verbose"}}
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("jv-worker")
	if err != nil {
		t.Fatal(err)
	}
	if proc.brokerGrant != "jv-worker" {
		t.Fatal("Registry.Launch did not go through the daemon")
	}
	got := f.backend(0).request(t).Config
	if got.PermissionMode != "plan" || len(got.ExtraArgs) != 1 || got.Name != "jv-worker" {
		t.Fatalf("Session-only fields did not reach the daemon: %+v", got)
	}
	dd := f.d.reg.Def("jv-worker")
	if dd == nil || dd.Purpose != PurposeWork || dd.Parent != "jevons-po" || !dd.AutoStart {
		t.Fatalf("daemon def = %+v", dd)
	}
	// AdoptOrLaunch on a second Registry (a restarted consumer) reclaims.
	reg2, _ := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	_ = reg2.Register(def)
	proc.mcpCleanup() // consumer 1 dies
	waitFor(t, "detached", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		return f.d.grants["jv-worker"].owner == nil
	})
	proc2, err := reg2.AdoptOrLaunch("jv-worker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg2.Stop("jv-worker") })
	if f.backend(1) != nil || proc2.SessionID() != "sid-jv" {
		t.Fatal("restarted consumer did not reclaim the running seat")
	}
}

// TestBrokerDaemonTaskRunStreamsThroughDaemon: Task.Run on a consumer runs
// on the daemon and the events come back in order; Cancel reaches it.
func TestBrokerDaemonTaskRunStreamsThroughDaemon(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	fake := &fakeTaskBackend{name: "fake-grok", events: []TaskEvent{
		{Type: TaskEventInit, SessionID: "run-sid", Model: "grok-4"},
		{Type: TaskEventText, Content: "part"},
		{Type: TaskEventResult, Content: "final", CostUSD: 0.01, Usage: Usage{InputTokens: 2}},
	}}
	prev := daemonNewTask
	daemonNewTask = func(cfg TaskConfig) *Task {
		if cfg.Provider != ProviderGrok || cfg.ID != "t-1" {
			t.Errorf("daemon task cfg = %+v", cfg)
		}
		tk := newTaskWithBackend(cfg, fake)
		tk.direct = true
		return tk
	}
	t.Cleanup(func() { daemonNewTask = prev })

	task := NewTask(TaskConfig{ID: "t-1", Provider: ProviderGrok, WorkDir: t.TempDir()})
	ch, err := task.Run(context.Background(), "summarise")
	if err != nil {
		t.Fatal(err)
	}
	var got []TaskEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 3 || got[2].Content != "final" || got[2].CostUSD != 0.01 || got[0].Model != "grok-4" {
		t.Fatalf("events via daemon = %+v", got)
	}
	if task.ClaudeID() != "run-sid" || task.LastResult() != "final" {
		t.Fatalf("task state not recorded: id=%q last=%q", task.ClaudeID(), task.LastResult())
	}
	if req := fake.requests[0]; req.Prompt != "summarise" {
		t.Fatalf("daemon ran prompt %q", req.Prompt)
	}
}

// TestBrokerDaemonUsageIsTheHostEvaluator is 🎯T2.9: consumers read the
// daemon's snapshot, the daemon fetches once per TTL, and a seat's stuck
// event forces a refresh.
func TestBrokerDaemonUsageIsTheHostEvaluator(t *testing.T) {
	pct := 40.0
	usage := []PlanUsage{{Provider: ProviderClaude, Status: PlanUsageAvailable,
		Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &pct}}}}
	f := startDaemon(t, false, usage)
	f.boot(t, false, usage)
	waitFor(t, "first fetch", func() bool { return f.fetchCount() == 1 })

	got, err := LoadPlanUsage(context.Background(), nil)
	if err != nil || len(got) != 1 || got[0].Provider != ProviderClaude {
		t.Fatalf("LoadPlanUsage via daemon = %+v, %v", got, err)
	}
	_, _ = LoadPlanUsage(context.Background(), nil)
	if f.fetchCount() != 1 {
		t.Fatalf("consumer reads caused vendor fetches: %d", f.fetchCount())
	}
	// Resolve reads the same snapshot.
	pick, err := Resolve(context.Background(), ModelPredicates{Mode: CapabilityTask, PreferProvider: ProviderClaude})
	if err != nil || pick.Provider != ProviderClaude {
		t.Fatalf("Resolve via daemon = %+v, %v", pick, err)
	}

	// A 429 on a seat invalidates the snapshot.
	a, err := Start(Config{Name: "seat-u", WorkDir: t.TempDir(), SessionID: "sid-u", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	f.d.reg.Get("seat-u").PublishEvent(Event{Type: "assistant", IsError: true, Text: "429 rate limit exceeded"})
	waitFor(t, "refetch after 429", func() bool { return f.fetchCount() >= 2 })

	// TTL elapses on the manual clock.
	before := f.fetchCount()
	f.clock.Advance(DefaultPlanCacheTTL + time.Second)
	_, _ = LoadPlanUsage(context.Background(), nil)
	if f.fetchCount() != before+1 {
		t.Fatalf("stale read did not refresh: %d → %d", before, f.fetchCount())
	}
}

// TestBrokerDaemonResumesSeatsOnBoot: seats held before the last stop come
// back — adopted when the process is still there, relaunched from the
// transcript otherwise — and a relaunched seat is told it was restarted.
func TestBrokerDaemonResumesSeatsOnBoot(t *testing.T) {
	f := startDaemon(t, true, nil)
	// A prior daemon held two seats and a released one.
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	prior := []AgentDef{
		{Name: "held-a", WorkDir: t.TempDir(), SessionID: "sid-a", AutoStart: true, Materialized: true, Provider: ProviderGrok},
		{Name: "held-b", WorkDir: t.TempDir(), SessionID: "sid-b", AutoStart: true},
		{Name: "released", WorkDir: t.TempDir(), SessionID: "sid-c"},
	}
	raw, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(f.state, grantsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f.boot(t, true, nil)

	waitFor(t, "two seats resumed", func() bool { return f.backend(1) != nil })
	waitFor(t, "nudges sent", func() bool {
		n := 0
		for i := 0; i < 2; i++ {
			b := f.backend(i)
			b.mu.Lock()
			if len(b.sends) == 1 && b.sends[0] == "restart-nudge" {
				n++
			}
			b.mu.Unlock()
		}
		return n == 2
	})
	if f.backend(2) != nil {
		t.Fatal("a released seat was resurrected")
	}
	names := map[string]bool{}
	for i := 0; i < 2; i++ {
		names[f.backend(i).request(t).Config.Name] = true
	}
	if !names["held-a"] || !names["held-b"] {
		t.Fatalf("resumed %v", names)
	}
	for i := 0; i < 2; i++ {
		if req := f.backend(i).request(t); req.Config.Name == "held-a" && !req.Config.RequireResume {
			t.Fatal("materialized seat relaunched without RequireResume")
		}
	}
	// The consumer comes back and reclaims what the daemon already resumed.
	a, err := Start(Config{Name: "held-a", Provider: ProviderGrok, WorkDir: prior[0].WorkDir, SessionID: "sid-a", RequireResume: true, TermLogPath: "-"})
	if err != nil {
		t.Fatalf("reclaim after boot resume: %v", err)
	}
	t.Cleanup(a.Stop)
	if f.backend(2) != nil {
		t.Fatal("consumer reclaim started a third process")
	}
}

// TestBrokerDaemonRemintsCursorSeatWhenLoadIsRefused: a standing AutoStart
// Cursor seat whose session/load is definitively refused is reminted onto
// a fresh session — not left dead for the owner to recover by hand.
func TestBrokerDaemonRemintsCursorSeatWhenLoadIsRefused(t *testing.T) {
	f := startDaemon(t, true, nil)
	old := "b54f134f-f7ef-4780-a077-37132cd64d14"
	var starts int
	prev := registryStartDirect
	registryStartDirect = func(ctx context.Context, cfg Config) (*Agent, error) {
		starts++
		if cfg.RequireResume {
			return nil, fmt.Errorf("acp session/load %s: Invalid params (%w)", cfg.SessionID, ErrCursorResumeDenied)
		}
		return prev(ctx, cfg)
	}
	writeGrantsTable(t, f.state, []AgentDef{{
		Name: "jevons-po", WorkDir: t.TempDir(), SessionID: old,
		AutoStart: true, Materialized: true, Provider: ProviderCursor,
	}})
	f.boot(t, true, nil)
	waitFor(t, "reminted seat alive", func() bool { return f.backend(0) != nil })
	if starts < 2 {
		t.Fatalf("starts = %d, want resume-fail then remint", starts)
	}
	def := f.d.reg.Def("jevons-po")
	if def == nil {
		t.Fatal("reminted seat missing from registry")
	}
	if def.SessionID == "" || def.SessionID == old {
		t.Fatalf("seat kept the unresumable session %q", def.SessionID)
	}
	if f.d.reg.Get("jevons-po") == nil {
		t.Fatal("reminted seat has no process")
	}
}

// TestBrokerDaemonBareServerFallsThroughToDirect: a protocol server with no
// daemon runtime answers not_available and the library takes the direct
// path (the 🎯T2.1 consult, preserved).
func TestBrokerDaemonBareServerFallsThroughToDirect(t *testing.T) {
	srv := startLibraryBroker(t)
	backend := &fakeAgentBackend{name: "fake-claude"}
	a, err := startConsideringBroker(Config{WorkDir: t.TempDir(), SessionID: "bare", TermLogPath: "-"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if a.brokerGrant != "" {
		t.Fatal("bare server produced a broker-held handle")
	}
	if srv.RequestCount() == 0 {
		t.Fatal("bare server was never consulted")
	}
	backend.request(t)
	if BrokerAvailable() {
		t.Fatal("BrokerAvailable true against a bare protocol server")
	}
}

// TestBrokerWireMirrorsAreComplete is the 🎯T24 rule applied to the socket:
// every Config field either rides on AgentDef or is named as deliberately
// local; every ModelPredicates field rides or is named. Event, TaskEvent,
// TaskConfig and ModelPick are pinned by struct conversion at compile time.
func TestBrokerWireMirrorsAreComplete(t *testing.T) {
	defFields := map[string]bool{}
	for f := range reflect.TypeFor[AgentDef]().Fields() {
		defFields[f.Name] = true
	}
	for f := range reflect.TypeFor[Config]().Fields() {
		if defFields[f.Name] || f.Name == "RequireResume" {
			continue
		}
		_, local := configNotOnGrantWire[f.Name]
		_, callback := configByCallback[f.Name]
		if !local && !callback {
			t.Errorf("Config.%s is neither on AgentDef (the grant wire), declared local in configNotOnGrantWire, nor honoured by callback in configByCallback", f.Name)
		}
		if local && callback {
			t.Errorf("Config.%s is declared both not carried and honoured by callback", f.Name)
		}
	}
	for _, declared := range []map[string]string{configNotOnGrantWire, configByCallback} {
		for name := range declared {
			if _, ok := reflect.TypeFor[Config]().FieldByName(name); !ok {
				t.Errorf("%s is declared off the grant wire, but Config no longer has it", name)
			}
		}
	}
	predFields := map[string]bool{}
	for f := range reflect.TypeFor[predicatesWire]().Fields() {
		predFields[f.Name] = true
	}
	for f := range reflect.TypeFor[ModelPredicates]().Fields() {
		if predFields[f.Name] {
			continue
		}
		if _, ok := predicatesNotOnWire[f.Name]; !ok {
			t.Errorf("ModelPredicates.%s is not on predicatesWire and not declared daemon-supplied", f.Name)
		}
	}
	// Wire codecs round-trip.
	ev := Event{Type: "assistant", Text: "x", Raw: []byte(`{"a":1}`), Usage: Usage{InputTokens: 1},
		WarningCodes: []string{"w"}, StuckClass: StuckClassQuota, FromProvider: ProviderGrok}
	raw, err := encodeEventWire(ev)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeEventWire(raw)
	if err != nil || !reflect.DeepEqual(back, ev) {
		t.Fatalf("event round trip: %+v vs %+v (%v)", back, ev, err)
	}
	if strings.Contains(string(raw), `"turn_id"`) {
		t.Fatalf("empty fields must be omitted on the wire: %s", raw)
	}
}

func TestDecodePredicatesWireSkillAlias(t *testing.T) {
	got, err := decodePredicatesWire([]byte(`{"skill":"analysis","quality":"standard"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Purpose != ModelPurposeAnalysis || got.Skill != ModelPurposeAnalysis {
		t.Fatalf("skill alias: %+v", got)
	}
	raw, err := encodePredicatesWire(ModelPredicates{Skill: ModelPurposeAnalysis})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"purpose":"analysis"`) {
		t.Fatalf("encode must materialize purpose from skill: %s", raw)
	}
}

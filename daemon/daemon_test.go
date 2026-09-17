// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// TestGrantIsDifferentialWithDirectStart is 🎯T2.10's acceptance: the same
// stub, once through the daemon and once direct, yields the same Event
// sequence on the consumer's handle, and Send / Interrupt / Resize reach the
// provider.
func TestGrantIsDifferentialWithDirectStart(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	turn := []claudia.Event{
		{Type: "assistant", Text: "hello", Model: "claude-opus-5", Usage: claudia.Usage{InputTokens: 3, OutputTokens: 5}},
		{Type: "progress", ProgressType: "tool_use", ToolCallID: "t1", ToolTitle: "Read"},
		{Type: "assistant", Text: "done", StopReason: "end_turn"},
	}

	// Through the daemon.
	a, err := claudia.Start(claudia.Config{Name: "seat-1", WorkDir: t.TempDir(), SessionID: "sid-1", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start via daemon: %v", err)
	}
	t.Cleanup(a.Stop)
	if !a.DaemonHeld() {
		t.Fatal("handle is not daemon-held")
	}
	daemonSide := f.seat(0)
	if daemonSide == nil {
		t.Fatal("daemon started no provider process")
	}
	if st := daemonSide.start(); st.Config.Name != "seat-1" || st.SessionID != "sid-1" {
		t.Fatalf("daemon-side start = %+v", st)
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
	interrupts, resizes, _ := daemonSide.counts()
	if sends := daemonSide.sent(); len(sends) != 1 || sends[0] != "hi" || interrupts != 1 || resizes != 1 {
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

	// Direct, same stub.
	b, err := claudia.StartStub(context.Background(), claudia.Config{WorkDir: t.TempDir(), SessionID: "sid-2", TermLogPath: "-"}, newSeat().ops())
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

// TestSendModeDispatch is 🎯T72.3's daemon clause, over the raw wire so the
// sent response is read as bytes rather than through the handle: each mode
// reaches the verb the design maps it to, and the response names the
// mechanism that ran. The stub flips itself busy on its first send, so
// phase_before moves from idle to in_turn as it would on a real seat.
func TestSendModeDispatch(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	c := rawSeat(t, f.sock, "modes")
	s := f.seat(0)
	if s == nil {
		t.Fatal("daemon started no provider process")
	}
	counts := func() ([]string, int) {
		interrupts, _, _ := s.counts()
		return s.sent(), interrupts
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
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeSteer, Mechanism: claudia.MechanismSubmit, PhaseBefore: string(claudia.TurnIdle)})
	// Absent mode is submit: today's client keeps today's behaviour.
	expect("submit", send("s1", "one", ""),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeSubmit, Mechanism: claudia.MechanismSubmit, PhaseBefore: string(claudia.TurnInTurn)})
	// Steer with a turn open runs the provider's steer, not Send.
	expect("steer", send("st1", "also", broker.SendModeSteer),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeSteer, Mechanism: "fake_steer", PhaseBefore: string(claudia.TurnInTurn), SupersededTurnID: "t-1"})
	sends, interrupts := counts()
	if !reflect.DeepEqual(sends, []string{"first", "one"}) || interrupts != 0 || !reflect.DeepEqual(s.steered(), []string{"also"}) {
		t.Fatalf("after steer: sends=%v interrupts=%d steers=%v", sends, interrupts, s.steered())
	}
	// Interrupt with a turn open hard-stops, then submits.
	expect("interrupt", send("i1", "stop", broker.SendModeInterrupt),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeInterrupt, Mechanism: claudia.MechanismInterruptThenSubmit, PhaseBefore: string(claudia.TurnInTurn)})
	sends, interrupts = counts()
	if !reflect.DeepEqual(sends, []string{"first", "one", "stop"}) || interrupts != 1 {
		t.Fatalf("after interrupt: sends=%v interrupts=%d", sends, interrupts)
	}
	// Queue is ack-only: the seat never sees the text.
	expect("queue", send("q1", "later", broker.SendModeQueue),
		&broker.SentResponse{Name: "modes", Mode: broker.SendModeQueue, Mechanism: claudia.MechanismClientQueue, PhaseBefore: string(claudia.TurnInTurn)})
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
	if len(sends) != 3 || interrupts != 2 || len(s.steered()) != 1 {
		t.Errorf("seat saw sends=%v interrupts=%d steers=%v after queue, interrupt and a refused mode", sends, interrupts, s.steered())
	}

	// turn_caps rides agent_info, and the grants snapshot, from the seat.
	want := &broker.TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: string(claudia.SteerBreakpoint), BusyOnSecondSubmit: claudia.BusySubmitSupersede}
	info := rawCall(t, c, &broker.Request{ID: "ai", Type: broker.TypeAgentInfo, AgentInfo: &broker.NamedRequest{Name: "modes"}})
	if info.AgentInfo == nil || !reflect.DeepEqual(info.AgentInfo.TurnCaps, want) {
		t.Errorf("agent_info turn_caps = %+v, want %+v", info.AgentInfo, want)
	}
	if got := f.d.grantList(); len(got) != 1 || !reflect.DeepEqual(got[0].TurnCaps, want) {
		t.Errorf("grants turn_caps = %+v, want %+v", got, want)
	}
}

// TestHandleForwardsSteerAndTurnCaps is the client half of 🎯T72.3: a
// daemon-held handle's Steer, SendMode and TurnCaps reach the daemon's seat
// through send.mode and turn_caps, with the daemon's outcome on the handle,
// rather than answering from the provider contract alone.
func TestHandleForwardsSteerAndTurnCaps(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	a, err := claudia.Start(claudia.Config{Name: "seat-m", WorkDir: t.TempDir(), SessionID: "sid-m", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	s := f.seat(0)

	if caps := a.TurnCaps(); caps != seatCaps {
		t.Fatalf("TurnCaps via daemon = %+v, want %+v", caps, seatCaps)
	}
	if got := f.d.reg.Get("seat-m").TurnPhase(); got != claudia.TurnIdle {
		t.Fatalf("seat phase = %s before any send", got)
	}
	if _, err := a.Steer("early"); !errors.Is(err, claudia.ErrTurnIdle) {
		t.Fatalf("Steer on an idle seat: err = %v, want ErrTurnIdle", err)
	}
	if err := a.Send("go"); err != nil {
		t.Fatal(err)
	}
	out, err := a.Steer("and this")
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if out.Mechanism != "fake_steer" || out.SupersededTurnID != "t-1" || out.PhaseBefore != claudia.TurnInTurn || out.Mode != claudia.DeliverySteer {
		t.Fatalf("Steer outcome = %+v", out)
	}
	if !reflect.DeepEqual(s.steered(), []string{"and this"}) {
		t.Fatalf("seat steers = %v", s.steered())
	}
	out, err = a.SendMode("now", claudia.DeliveryInterrupt)
	if err != nil {
		t.Fatalf("SendMode interrupt: %v", err)
	}
	if out.Mechanism != claudia.MechanismInterruptThenSubmit {
		t.Fatalf("interrupt outcome = %+v", out)
	}
	interrupts, _, _ := s.counts()
	if sends := s.sent(); !reflect.DeepEqual(sends, []string{"go", "now"}) || interrupts != 1 {
		t.Fatalf("seat saw sends=%v interrupts=%d", sends, interrupts)
	}
}

// TestDeliverSendOnStubAgent is the dispatch table on a stub whose every
// verb is observable (🎯T72.3): each wire mode runs exactly the Agent verb
// the design maps it to, the sent response carries the mechanism the verb
// reported, and turn_caps is the stub's own answer. The suite above proves
// the same over the socket; this pins the table without one.
func TestDeliverSendOnStubAgent(t *testing.T) {
	var ran []string
	phase := claudia.TurnInTurn
	stub := claudia.NewStubAgentOps(&claudia.StubAgentOps{
		Provider: claudia.ProviderCursor,
		Send:     func(text string) error { ran = append(ran, "send:"+text); return nil },
		Steer: func(text string) (claudia.DeliveryOutcome, error) {
			ran = append(ran, "steer:"+text)
			return claudia.DeliveryOutcome{Mechanism: "stub_steer", SupersededTurnID: "t-2"}, nil
		},
		Interrupt: func() error { ran = append(ran, "interrupt"); phase = claudia.TurnIdle; return nil },
		TurnPhase: func() claudia.TurnPhase { return phase },
		TurnCaps:  func() claudia.TurnCaps { return seatCaps },
	})
	cases := []struct {
		mode broker.SendMode
		text string
		want *broker.SentResponse
		ran  []string
	}{
		{broker.SendModeSteer, "fold", &broker.SentResponse{Mode: broker.SendModeSteer, Mechanism: "stub_steer", PhaseBefore: "in_turn", SupersededTurnID: "t-2"}, []string{"steer:fold"}},
		{broker.SendModeQueue, "hold", &broker.SentResponse{Mode: broker.SendModeQueue, Mechanism: claudia.MechanismClientQueue, PhaseBefore: "in_turn"}, nil},
		{broker.SendModeInterrupt, "stop", &broker.SentResponse{Mode: broker.SendModeInterrupt, Mechanism: claudia.MechanismInterruptThenSubmit, PhaseBefore: "in_turn"}, []string{"interrupt", "send:stop"}},
		{broker.SendModeSubmit, "go", &broker.SentResponse{Mode: broker.SendModeSubmit, Mechanism: claudia.MechanismSubmit, PhaseBefore: "idle"}, []string{"send:go"}},
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
	want := &broker.TurnCaps{CanInterrupt: true, CanSteer: true, SteerPolicy: string(claudia.SteerBreakpoint), BusyOnSecondSubmit: claudia.BusySubmitSupersede}
	if got := turnCapsWire(stub); !reflect.DeepEqual(got, want) {
		t.Errorf("turn_caps = %+v, want %+v", got, want)
	}
}

// TestTurnCapsMirrorIsComplete keeps broker.TurnCaps field for field with
// claudia.TurnCaps, the way the other wire mirrors are held (🎯T24: a field
// added to one side without the other would vanish on the socket).
func TestTurnCapsMirrorIsComplete(t *testing.T) {
	wire := map[string]bool{}
	for f := range reflect.TypeFor[broker.TurnCaps]().Fields() {
		wire[f.Name] = true
	}
	for f := range reflect.TypeFor[claudia.TurnCaps]().Fields() {
		if !wire[f.Name] {
			t.Errorf("TurnCaps.%s has no slot on broker.TurnCaps", f.Name)
		}
		delete(wire, f.Name)
	}
	for name := range wire {
		t.Errorf("broker.TurnCaps.%s is not a TurnCaps field", name)
	}
}

// TestReclaimAfterConsumerRestart is 🎯T2.11: the consumer's connection dies
// without a release, the seat keeps running, what it says meanwhile is
// retained, and a new consumer reclaims it by name with that history
// replayed ahead of live traffic.
func TestReclaimAfterConsumerRestart(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	first, err := claudia.Start(claudia.Config{Name: "seat-r", WorkDir: t.TempDir(), SessionID: "sid-r", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	proc := f.d.reg.Get("seat-r")
	// The consumer goes away without releasing.
	if err := first.Detach(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "seat detached", func() bool { return !f.owned("seat-r") && f.d.grants["seat-r"] != nil })
	if !proc.Alive() {
		t.Fatal("seat was stopped when the consumer went away")
	}
	waitFor(t, "handle marked unreachable", func() bool { return !first.Alive() })

	proc.PublishEvent(claudia.Event{Type: "assistant", Text: "while you were away", StopReason: "end_turn"})
	proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: "Bash"})

	second, err := claudia.Start(claudia.Config{Name: "seat-r", WorkDir: t.TempDir(), SessionID: "sid-r", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	t.Cleanup(second.Stop)
	if second.SessionID() != "sid-r" {
		t.Fatalf("reclaimed a different session: %s", second.SessionID())
	}
	if f.seat(1) != nil {
		t.Fatal("reclaim started a second provider process")
	}
	got := collectEvents(second)
	proc.PublishEvent(claudia.Event{Type: "assistant", Text: "live", StopReason: "end_turn"})
	waitFor(t, "replay + live", func() bool { return len(got()) >= 3 })
	evs := got()
	if evs[0].Text != "while you were away" || evs[1].ToolTitle != "Bash" || evs[2].Text != "live" {
		t.Fatalf("replay order wrong: %+v", evs)
	}

	// A third consumer cannot steal a held seat.
	_, err = claudia.Start(claudia.Config{Name: "seat-r", WorkDir: t.TempDir(), SessionID: "sid-r", TermLogPath: "-"})
	var pe *broker.ProtocolError
	if !errors.As(err, &pe) || pe.Code != broker.CodeGrantHeld {
		t.Fatalf("second owner: err = %v, want grant_held", err)
	}
}

// TestStopReleasesTheSeat: Stop on the handle tears the seat down on the
// daemon and forgets it.
func TestStopReleasesTheSeat(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	a, err := claudia.Start(claudia.Config{Name: "seat-s", WorkDir: t.TempDir(), SessionID: "sid-s", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	a.Stop()
	waitFor(t, "seat removed", func() bool { return f.d.reg.Def("seat-s") == nil })
	if _, _, stops := f.seat(0).counts(); stops != 1 {
		t.Fatalf("provider stops = %d", stops)
	}
}

// TestRegistryLaunchGoesThroughDaemon: a consumer Registry (Jevons's
// surface) launches by name and the daemon owns the process.
func TestRegistryLaunchGoesThroughDaemon(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	reg, err := claudia.NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	def := claudia.AgentDef{Name: "jv-worker", WorkDir: t.TempDir(), SessionID: "sid-jv", Purpose: claudia.PurposeWork, Parent: "jevons-po",
		PermissionMode: "plan", ExtraArgs: []string{"--verbose"}}
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("jv-worker")
	if err != nil {
		t.Fatal(err)
	}
	if !proc.DaemonHeld() {
		t.Fatal("Registry.Launch did not go through the daemon")
	}
	got := f.seat(0).start().Config
	if got.PermissionMode != "plan" || len(got.ExtraArgs) != 1 || got.Name != "jv-worker" {
		t.Fatalf("Session-only fields did not reach the daemon: %+v", got)
	}
	dd := f.d.reg.Def("jv-worker")
	if dd == nil || dd.Purpose != claudia.PurposeWork || dd.Parent != "jevons-po" || !dd.AutoStart {
		t.Fatalf("daemon def = %+v", dd)
	}
	// AdoptOrLaunch on a second Registry (a restarted consumer) reclaims.
	reg2, _ := claudia.NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	_ = reg2.Register(def)
	if err := proc.Detach(); err != nil { // consumer 1 goes away
		t.Fatal(err)
	}
	waitFor(t, "detached", func() bool { return !f.owned("jv-worker") })
	proc2, err := reg2.AdoptOrLaunch("jv-worker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg2.Stop("jv-worker") })
	if f.seat(1) != nil || proc2.SessionID() != "sid-jv" {
		t.Fatal("restarted consumer did not reclaim the running seat")
	}
}

// TestTaskRunStreamsThroughDaemon: Task.Run on a consumer runs on the daemon
// and the events come back in order.
func TestTaskRunStreamsThroughDaemon(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	var prompts []string
	prev := daemonNewTask
	daemonNewTask = func(cfg claudia.TaskConfig) *claudia.Task {
		if cfg.Provider != claudia.ProviderGrok || cfg.ID != "t-1" {
			t.Errorf("daemon task cfg = %+v", cfg)
		}
		return claudia.NewStubTask(cfg, &claudia.StubTaskOps{Run: func(_ context.Context, run claudia.StubTaskRun) (<-chan claudia.TaskEvent, error) {
			prompts = append(prompts, run.Prompt)
			ch := make(chan claudia.TaskEvent, 3)
			ch <- claudia.TaskEvent{Type: claudia.TaskEventInit, SessionID: "run-sid", Model: "grok-4"}
			ch <- claudia.TaskEvent{Type: claudia.TaskEventText, Content: "part"}
			ch <- claudia.TaskEvent{Type: claudia.TaskEventResult, Content: "final", CostUSD: 0.01, Usage: claudia.Usage{InputTokens: 2}}
			close(ch)
			return ch, nil
		}})
	}
	t.Cleanup(func() { daemonNewTask = prev })

	task := claudia.NewTask(claudia.TaskConfig{ID: "t-1", Provider: claudia.ProviderGrok, WorkDir: t.TempDir()})
	ch, err := task.Run(context.Background(), "summarise")
	if err != nil {
		t.Fatal(err)
	}
	var got []claudia.TaskEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 3 || got[2].Content != "final" || got[2].CostUSD != 0.01 || got[0].Model != "grok-4" {
		t.Fatalf("events via daemon = %+v", got)
	}
	if task.ClaudeID() != "run-sid" || task.LastResult() != "final" {
		t.Fatalf("task state not recorded: id=%q last=%q", task.ClaudeID(), task.LastResult())
	}
	if len(prompts) != 1 || prompts[0] != "summarise" {
		t.Fatalf("daemon ran prompts %q", prompts)
	}
}

// TestUsageIsTheHostEvaluator is 🎯T2.9: consumers read the daemon's
// snapshot, the daemon fetches once per TTL, and a seat's stuck event forces
// a refresh.
func TestUsageIsTheHostEvaluator(t *testing.T) {
	pct := 40.0
	usage := []claudia.PlanUsage{{Provider: claudia.ProviderClaude, Status: claudia.PlanUsageAvailable,
		Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &pct}}}}
	f := newFixture(t)
	f.boot(t, usage)
	waitFor(t, "first fetch", func() bool { return f.fetchCount() == 1 })

	got, err := claudia.LoadPlanUsage(context.Background(), nil)
	if err != nil || len(got) != 1 || got[0].Provider != claudia.ProviderClaude {
		t.Fatalf("LoadPlanUsage via daemon = %+v, %v", got, err)
	}
	_, _ = claudia.LoadPlanUsage(context.Background(), nil)
	if f.fetchCount() != 1 {
		t.Fatalf("consumer reads caused vendor fetches: %d", f.fetchCount())
	}
	// Resolve reads the same snapshot.
	pick, err := claudia.Resolve(context.Background(), claudia.ModelPredicates{Mode: claudia.CapabilityTask, PreferProvider: claudia.ProviderClaude})
	if err != nil || pick.Provider != claudia.ProviderClaude {
		t.Fatalf("Resolve via daemon = %+v, %v", pick, err)
	}

	// A 429 on a seat invalidates the snapshot.
	a, err := claudia.Start(claudia.Config{Name: "seat-u", WorkDir: t.TempDir(), SessionID: "sid-u", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	f.d.reg.Get("seat-u").PublishEvent(claudia.Event{Type: "assistant", IsError: true, Text: "429 rate limit exceeded"})
	waitFor(t, "refetch after 429", func() bool { return f.fetchCount() >= 2 })

	// TTL elapses on the manual clock.
	before := f.fetchCount()
	f.clock.Advance(claudia.DefaultPlanCacheTTL + time.Second)
	_, _ = claudia.LoadPlanUsage(context.Background(), nil)
	if f.fetchCount() != before+1 {
		t.Fatalf("stale read did not refresh: %d → %d", before, f.fetchCount())
	}
}

// TestResumesSeatsOnBoot: seats held before the last stop come back —
// relaunched from the transcript when their process is gone — a relaunched
// seat is told it was restarted, and a released seat stays released.
func TestResumesSeatsOnBoot(t *testing.T) {
	f := newFixture(t)
	prior := []claudia.AgentDef{
		{Name: "held-a", WorkDir: t.TempDir(), SessionID: "sid-a", AutoStart: true, Materialized: true, Provider: claudia.ProviderGrok},
		{Name: "held-b", WorkDir: t.TempDir(), SessionID: "sid-b", AutoStart: true},
		{Name: "released", WorkDir: t.TempDir(), SessionID: "sid-c"},
	}
	writeGrantsTable(t, f.state, prior)
	f.bootWith(t, f.options(nil))

	waitFor(t, "two seats resumed", func() bool { return f.seat(1) != nil })
	waitFor(t, "nudges sent", func() bool {
		n := 0
		for i := 0; i < 2; i++ {
			if sends := f.seat(i).sent(); len(sends) == 1 && sends[0] == "restart-nudge" {
				n++
			}
		}
		return n == 2
	})
	if f.seat(2) != nil {
		t.Fatal("a released seat was resurrected")
	}
	names := map[string]bool{}
	for i := 0; i < 2; i++ {
		cfg := f.seat(i).start().Config
		names[cfg.Name] = true
		if cfg.Name == "held-a" && !cfg.RequireResume {
			t.Fatal("materialized seat relaunched without RequireResume")
		}
	}
	if !names["held-a"] || !names["held-b"] {
		t.Fatalf("resumed %v", names)
	}
	// The consumer comes back and reclaims what the daemon already resumed.
	a, err := claudia.Start(claudia.Config{Name: "held-a", Provider: claudia.ProviderGrok, WorkDir: prior[0].WorkDir, SessionID: "sid-a", RequireResume: true, TermLogPath: "-"})
	if err != nil {
		t.Fatalf("reclaim after boot resume: %v", err)
	}
	t.Cleanup(a.Stop)
	if f.seat(2) != nil {
		t.Fatal("consumer reclaim started a third process")
	}
}

// TestRemintsCursorSeatWhenLoadIsRefused: a standing AutoStart Cursor seat
// whose session/load is definitively refused is reminted onto a fresh
// session — not left dead for the owner to recover by hand.
func TestRemintsCursorSeatWhenLoadIsRefused(t *testing.T) {
	f := newFixture(t)
	old := "b54f134f-f7ef-4780-a077-37132cd64d14"
	var starts int
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		starts++
		if cfg.RequireResume {
			return nil, fmt.Errorf("acp session/load %s: Invalid params (%w)", cfg.SessionID, claudia.ErrCursorResumeDenied)
		}
		return f.startSeat(ctx, cfg)
	}
	writeGrantsTable(t, f.state, []claudia.AgentDef{{
		Name: "jevons-po", WorkDir: t.TempDir(), SessionID: old,
		AutoStart: true, Materialized: true, Provider: claudia.ProviderCursor,
	}})
	f.bootWith(t, f.options(nil))
	waitFor(t, "reminted seat alive", func() bool { return f.seat(0) != nil && f.d.reg.Get("jevons-po") != nil })
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
}

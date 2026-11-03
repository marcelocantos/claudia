// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// fakeFleetSeat answers each Send with reply, or holds the turn until released.
type fakeFleetSeat struct {
	reply func(text string) (string, bool)

	mu      sync.Mutex
	sent    []string
	replies chan string
	gone    chan struct{}
	once    sync.Once
	stops   int
	death   chan error
	dead    atomic.Bool
	// dieOnSend makes the seat die as soon as it is given a turn.
	dieOnSend error
	// failTurn makes a turn fail while the seat stays alive.
	failTurn error
	// stopHold, if set, keeps Stop from returning until it closes.
	stopHold chan struct{}
	// usage is what the seat reports; turnTokens is added on each turn.
	usage      Usage
	turnTokens int
}

func newFakeFleetSeat(reply func(string) (string, bool)) *fakeFleetSeat {
	return &fakeFleetSeat{reply: reply, replies: make(chan string, 16), gone: make(chan struct{}), death: make(chan error, 1)}
}

func fleetAnswer(text string) func(string) (string, bool) {
	return func(string) (string, bool) { return text, true }
}

func fleetHold(string) (string, bool) { return "", false }

func (s *fakeFleetSeat) Send(text string) error {
	s.mu.Lock()
	s.sent = append(s.sent, text)
	s.usage.OutputTokens += s.turnTokens
	s.mu.Unlock()
	if s.dieOnSend != nil {
		s.dead.Store(true)
		return s.dieOnSend
	}
	if s.failTurn != nil {
		s.death <- s.failTurn
		return nil
	}
	if r, ok := s.reply(text); ok {
		s.replies <- r
	}
	return nil
}

func (s *fakeFleetSeat) WaitForResponse(ctx context.Context) (string, error) {
	select {
	case r := <-s.replies:
		return r, nil
	case <-s.gone:
		return "", nil
	case err := <-s.death:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *fakeFleetSeat) Stop() {
	s.mu.Lock()
	s.stops++
	s.mu.Unlock()
	if s.stopHold != nil {
		<-s.stopHold
	}
	s.once.Do(func() { close(s.gone) })
}
func (s *fakeFleetSeat) Alive() bool { return !s.dead.Load() }

// die ends the seat's process during its turn.
func (s *fakeFleetSeat) die(err error) {
	s.dead.Store(true)
	s.death <- err
}
func (s *fakeFleetSeat) Describe() string { return "fake/model" }
func (s *fakeFleetSeat) Usage() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}
func (s *fakeFleetSeat) LastActivity() time.Time { return time.Now() }

func (s *fakeFleetSeat) sends() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// fleetRig runs a kernel over a shared table of seats, which survives reopening
// the way a sidecar's seats survive a broker restart.
type fleetRig struct {
	t   *testing.T
	dir string
	k   *FleetKernel

	mu      sync.Mutex
	seats   map[string]*fakeFleetSeat
	specs   []FleetSeatSpec
	startFn func(ctx context.Context, spec *FleetSeatSpec) (FleetSeat, error)

	discarded []string
	// clock, when set before open, is the kernel's clock.
	clock Clock
}

func (r *fleetRig) setStartFn(fn func(ctx context.Context, spec *FleetSeatSpec) (FleetSeat, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startFn = fn
}

func (r *fleetRig) setSeat(name string, seat *fakeFleetSeat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seats[name] = seat
}

func (r *fleetRig) specCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.specs)
}

func newFleetRig(t *testing.T, seats map[string]*fakeFleetSeat) *fleetRig {
	t.Helper()
	r := &fleetRig{t: t, dir: t.TempDir(), seats: seats}
	r.open()
	if err := r.k.Open("f", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.k.Close() })
	return r
}

func (r *fleetRig) open() {
	r.t.Helper()
	k, err := OpenFleetKernel(&FleetKernelArgs{Dir: r.dir, Clock: r.clock, RetryWait: time.Millisecond, Discard: func(_, name string) {
		r.mu.Lock()
		r.discarded = append(r.discarded, name)
		r.mu.Unlock()
	}, Start: func(ctx context.Context, spec *FleetSeatSpec) (FleetSeat, error) {
		r.mu.Lock()
		r.specs = append(r.specs, *spec)
		fn := r.startFn
		seat := r.seats[spec.Name]
		r.mu.Unlock()
		if fn != nil {
			return fn(ctx, spec)
		}
		if seat == nil {
			return nil, errors.New("no such seat")
		}
		return seat, nil
	}})
	if err != nil {
		r.t.Fatal(err)
	}
	k.Resume()
	r.k = k
}

func (r *fleetRig) start(name, parent string) {
	r.t.Helper()
	err := r.k.Start(context.Background(), "f", parent, &FleetStartArgs{Name: name, Instructions: "BRIEF-" + name, WorkDir: "/tmp"})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *fleetRig) send(from, to, text string) string {
	r.t.Helper()
	id, err := r.k.Send("f", &FleetSendArgs{From: from, To: to, Kind: FleetRequest, Text: text})
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// ownerMessage waits for the next message to the owner after *seq.
func (r *fleetRig) ownerMessage(seq *uint64) *FleetMessage {
	r.t.Helper()
	ctx := wallclockguard.UntilTestTimeout(r.t)
	for {
		events, err := r.k.Read(ctx, "f", *seq)
		if err != nil {
			r.t.Fatal(err)
		}
		if events == nil {
			r.t.Fatal("no message to the owner before the test timed out")
		}
		for i := range events {
			*seq = events[i].Seq
			if m := events[i].Message; m != nil && m.To == FleetOwner {
				return m
			}
		}
	}
}

// waitFleet waits on the condition, with go test -timeout as the clock.
func waitFleet(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ctx := wallclockguard.UntilTestTimeout(t)
	for !cond() {
		if ctx.Err() != nil {
			t.Fatal("test timed out waiting for " + what)
		}
		time.Sleep(time.Millisecond)
	}
}

func fleetSeatHeard(seat *fakeFleetSeat, text string) bool {
	for _, sent := range seat.sends() {
		if strings.Contains(sent, text) {
			return true
		}
	}
	return false
}

func TestFleetTurnTextAnswersARequest(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("the answer"))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	id := r.send(FleetOwner, "a", "question")
	var seq uint64
	m := r.ownerMessage(&seq)
	if m.Text != "the answer" || m.InReplyTo != id || m.From != "a" || m.Kind != FleetResponse {
		t.Fatalf("reply = %+v", m)
	}
	got := seat.sends()
	if len(got) != 1 || !strings.Contains(got[0], "BRIEF-a") || !strings.Contains(got[0], "question") {
		t.Fatalf("first delivery lacks brief or message: %q", got)
	}
	r.send(FleetOwner, "a", "second")
	waitFleet(t, "second send", func() bool { return len(seat.sends()) == 2 })
	if got := seat.sends()[1]; strings.Contains(got, "BRIEF-a") {
		t.Fatalf("brief repeated: %q", got)
	}
}

func TestFleetEveryRequestGetsAResponseWhenItsTargetDies(t *testing.T) {
	cases := []struct {
		name string
		kill func(*fleetRig)
	}{
		{"stopped mid-turn", func(r *fleetRig) {
			if err := r.k.Stop("f", FleetOwner, "b"); err != nil {
				r.t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent, target := newFakeFleetSeat(fleetAnswer("parent saw it")), newFakeFleetSeat(fleetHold)
			r := newFleetRig(t, map[string]*fakeFleetSeat{"a": parent, "b": target})
			r.start("a", FleetOwner)
			r.start("b", "a")
			r.send("a", "b", "work")
			waitFleet(t, "turn", func() bool { return len(target.sends()) == 1 })
			c.kill(r)
			waitFleet(t, "failure to reach the requester", func() bool {
				for _, text := range parent.sends() {
					if strings.Contains(text, "agent b is gone") {
						return true
					}
				}
				return false
			})
		})
	}
}

func TestFleetRequestToAGoneAgentIsAnsweredWithAFailure(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	// A timer outlives its target, which is the case a Send check cannot catch.
	if _, err := r.k.Send("f", &FleetSendArgs{From: FleetOwner, To: "a", Kind: FleetRequest, Text: "later", After: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Stop("f", FleetOwner, "a"); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if m := r.ownerMessage(&seq); !strings.Contains(m.Text, "agent a is gone") {
		t.Fatalf("got %+v", m)
	}
}

func TestFleetUnsolicitedTurnsGoUpTheTreeAndNoReplyStaysSilent(t *testing.T) {
	child := newFakeFleetSeat(func(text string) (string, bool) {
		if strings.Contains(text, "quietly") {
			return "Thinking it over.\n" + FleetNoReply, true
		}
		return "progress", true
	})
	parent := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"p": parent, "c": child})
	r.start("p", FleetOwner)
	r.start("c", "p")
	if _, err := r.k.Send("f", &FleetSendArgs{From: FleetOwner, To: "c", Kind: FleetNotice, Text: "quietly"}); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "silent turn", func() bool { return len(child.sends()) == 1 })
	if _, err := r.k.Send("f", &FleetSendArgs{From: FleetOwner, To: "c", Kind: FleetNotice, Text: "report"}); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "report to reach the parent", func() bool { return len(parent.sends()) == 1 })
	if got := parent.sends(); !strings.Contains(got[0], "progress") {
		t.Fatalf("parent got %q", got)
	}
}

func TestFleetDelayedMessageArrivesThoughItsSenderIsIdle(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "wake up", After: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "timer", func() bool { return len(seat.sends()) == 1 })
	if got := seat.sends()[0]; !strings.Contains(got, "[timer") || !strings.Contains(got, "wake up") {
		t.Fatalf("got %q", got)
	}
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "now"}); err == nil {
		t.Fatal("an undelayed message to yourself was accepted")
	}
}

func TestFleetAuthorityIsScopedToYourOwnTree(t *testing.T) {
	seats := map[string]*fakeFleetSeat{}
	for _, name := range []string{"a", "a1", "a2", "b"} {
		seats[name] = newFakeFleetSeat(fleetHold)
	}
	r := newFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("a1", "a")
	r.start("a2", "a1")
	r.start("b", FleetOwner)
	if err := r.k.Stop("f", "a", "b"); err == nil {
		t.Fatal("an agent stopped another tree's agent")
	}
	if err := r.k.Stop("f", "a1", "a"); err == nil {
		t.Fatal("an agent stopped its parent")
	}
	if err := r.k.Stop("f", "a", "a2"); err != nil {
		t.Fatalf("grandparent could not stop a descendant: %v", err)
	}
	if err := r.k.Stop("f", "b", "b"); err != nil {
		t.Fatalf("an agent could not stop itself: %v", err)
	}
}

func TestFleetSeatGetsItsOwnTokenAndInheritsWorkDir(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold), "b": newFakeFleetSeat(fleetHold)}
	r := newFleetRig(t, seats)
	r.start("a", FleetOwner)
	if err := r.k.Start(context.Background(), "f", "a", &FleetStartArgs{Name: "b", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) != 2 || r.specs[0].Token == r.specs[1].Token || r.specs[0].Token == "" {
		t.Fatalf("tokens: %+v", r.specs)
	}
	if r.specs[1].WorkDir != "/tmp" || r.specs[1].Resume {
		t.Fatalf("child spec: %+v", r.specs[1])
	}
}

func TestFleetRefusals(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	k := r.k
	ctx := context.Background()
	if err := k.Open("f2", &FleetOptions{MaxAgents: 1, MaxMessages: 1}); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"bad fleet name":     k.Open("Bad Name", nil),
		"unknown fleet":      k.Start(ctx, "nope", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"}),
		"bad agent name":     k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "Bad Name", Instructions: "x", WorkDir: "/tmp"}),
		"doubled hyphen":     k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "a--b", Instructions: "x", WorkDir: "/tmp"}),
		"over-long name":     k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: strings.Repeat("a", fleetMaxName+1), Instructions: "x", WorkDir: "/tmp"}),
		"reserved name":      k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: FleetOwner, Instructions: "x", WorkDir: "/tmp"}),
		"no instructions":    k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "a", WorkDir: "/tmp"}),
		"relative workdir":   k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "rel"}),
		"root needs workdir": k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x"}),
		"unknown parent":     k.Start(ctx, "f", "ghost", &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"}),
		"missing brief file": k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "a", InstructionsFile: "/nonexistent", WorkDir: "/tmp"}),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := k.Start(ctx, "f2", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if err := k.Start(ctx, "f2", FleetOwner, &FleetStartArgs{Name: "a2", Instructions: "x", WorkDir: "/tmp"}); err == nil || !strings.Contains(err.Error(), "agent limit") {
		t.Fatalf("agent limit: %v", err)
	}
	if _, err := k.Send("f2", &FleetSendArgs{From: FleetOwner, To: "a", Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Send("f2", &FleetSendArgs{From: FleetOwner, To: "a", Text: "two"}); err == nil || !strings.Contains(err.Error(), "message limit") {
		t.Fatalf("message limit: %v", err)
	}
	if _, err := k.Send("f", &FleetSendArgs{From: FleetOwner, To: "ghost", Text: "x"}); err == nil {
		t.Fatal("sent to an unknown agent")
	}
	if _, err := k.Send("f", &FleetSendArgs{From: FleetOwner, To: "a", Kind: "response", Text: "x"}); err == nil {
		t.Fatal("a client forged a response")
	}
}

func TestFleetFailedStartFreesTheNameAndIsLogged(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"})
	if err == nil {
		t.Fatal("start with no seat succeeded")
	}
	st, err := r.k.Status("f")
	if err != nil || len(st.Agents) != 0 {
		t.Fatalf("status %+v %v", st, err)
	}
	r.setSeat("a", newFakeFleetSeat(fleetHold))
	r.start("a", FleetOwner)
	events, err := r.k.Read(wallclockguard.UntilTestTimeout(t), "f", 0)
	if err != nil || len(events) == 0 || events[0].Kind != FleetEventStartFailed {
		t.Fatalf("events %+v %v", events, err)
	}
}

func TestFleetRestartDeliversTheInterruptedTurnAgainAheadOfTheQueue(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	first := r.send(FleetOwner, "a", "interrupted work")
	r.send(FleetOwner, "a", "queued work")
	waitFleet(t, "turn", func() bool { return len(seat.sends()) == 1 })

	r.k.Close()
	if seat.stops != 0 {
		t.Fatal("closing the kernel stopped a seat")
	}
	next := newFakeFleetSeat(fleetAnswer("finished"))
	r.setSeat("a", next)
	r.open()
	var seq uint64
	var m *FleetMessage
	for m == nil || m.InReplyTo != first {
		m = r.ownerMessage(&seq)
	}
	if m.Text != "finished" {
		t.Fatalf("reply = %+v", m)
	}
	waitFleet(t, "queue to drain", func() bool { return len(next.sends()) == 2 })
	got := next.sends()
	if !strings.Contains(got[0], "interrupted work") || !strings.Contains(got[0], "Claudia restarted") || !strings.Contains(got[0], "BRIEF-a") {
		t.Fatalf("redelivery: %q", got[0])
	}
	if !strings.Contains(got[1], "queued work") || strings.Contains(got[1], "Claudia restarted") {
		t.Fatalf("queue: %q", got[1])
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.specs[len(r.specs)-1].Resume {
		t.Fatal("reattach did not ask to resume")
	}
}

func TestFleetRestartKeepsAgentsTimersAndHistory(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("before"))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "one")
	var seq uint64
	r.ownerMessage(&seq)
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "after the restart", After: 50 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	r.k.Close()

	next := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r.setSeat("a", next)
	r.open()
	waitFleet(t, "timer after restart", func() bool { return len(next.sends()) == 1 })
	if got := next.sends()[0]; !strings.Contains(got, "after the restart") {
		t.Fatalf("got %q", got)
	}
	// A client that was away reads what it missed, and sequence numbers go on.
	events, err := r.k.Read(wallclockguard.UntilTestTimeout(t), "f", 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("history %v %v", events, err)
	}
	last := uint64(0)
	found := false
	for _, e := range events {
		if e.Seq <= last {
			t.Fatalf("sequence went backwards at %+v", e)
		}
		last = e.Seq
		found = found || (e.Message != nil && e.Message.Text == "before")
	}
	if !found {
		t.Fatal("the owner's earlier message was lost")
	}
	st, err := r.k.Status("f")
	if err != nil || len(st.Agents) != 1 {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestFleetStartInterruptedByCloseIsReattached(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	entered := make(chan struct{})
	r.setStartFn(func(ctx context.Context, _ *FleetSeatSpec) (FleetSeat, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	failed := make(chan error, 1)
	go func() {
		failed <- r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"})
	}()
	<-entered
	r.k.Close()
	if err := <-failed; err == nil {
		t.Fatal("start survived close")
	}
	seat := newFakeFleetSeat(fleetAnswer("up"))
	r.setSeat("a", seat)
	r.setStartFn(nil)
	r.open()
	waitFleet(t, "reattach", func() bool {
		st, err := r.k.Status("f")
		return err == nil && len(st.Agents) == 1 && st.Agents[0].State == FleetStateIdle
	})
}

func TestFleetDamagedStateIsAnErrorAndLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet-state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenFleetKernel(&FleetKernelArgs{Dir: dir, Start: func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return nil, nil }})
	if err == nil || !strings.Contains(err.Error(), "damaged state") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{broken" {
		t.Fatalf("state rewritten: %q", raw)
	}
}

func TestFleetLogDropsACutOffLastLineButRefusesOtherDamage(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("ok"))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.k.Close()
	path := filepath.Join(r.dir, "f.events.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":99,"ki`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r.open()
	st, err := r.k.Status("f")
	if err != nil || st.LastSeq >= 99 {
		t.Fatalf("status %+v %v", st, err)
	}
	r.k.Close()

	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFleetKernel(&FleetKernelArgs{Dir: r.dir, Start: func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return nil, nil }}); err == nil {
		t.Fatal("damaged log accepted")
	}
}

func TestFleetReadReachesOlderEventsThanTheRingKeeps(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	for range fleetEventRing + 10 {
		r.k.mu.Lock()
		r.k.eventLocked("f", &FleetEvent{Kind: FleetEventStop})
		r.k.mu.Unlock()
	}
	events, err := r.k.Read(context.Background(), "f", 0)
	if err != nil || len(events) == 0 || events[0].Seq != 1 {
		t.Fatalf("first read: %d events, err %v", len(events), err)
	}
}

func TestFleetShutdownStopsEverythingAndForgetsTheFleet(t *testing.T) {
	a, b := newFakeFleetSeat(fleetHold), newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": a, "b": b})
	r.start("a", FleetOwner)
	r.start("b", "a")
	if err := r.k.Shutdown(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	for name, seat := range map[string]*fakeFleetSeat{"a": a, "b": b} {
		select {
		case <-seat.gone:
		default:
			t.Fatalf("seat %s left running", name)
		}
	}
	if _, err := r.k.Status("f"); err == nil {
		t.Fatal("fleet survived shutdown")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "f.events.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log left behind: %v", err)
	}
}

func TestFleetKernelHasNoFleetsUntilOneIsOpened(t *testing.T) {
	dir := t.TempDir()
	k, err := OpenFleetKernel(&FleetKernelArgs{Dir: dir, Start: func(context.Context, *FleetSeatSpec) (FleetSeat, error) {
		t.Error("a dormant kernel started a seat")
		return nil, errors.New("unexpected")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if _, err := k.Status("anything"); err == nil {
		t.Fatal("status of a fleet nobody opened")
	}
}

// fleetRPC makes one MCP call the way a seat's client does.
func fleetRPC(t *testing.T, srv *httptest.Server, token, method string, params any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/upstream/fleet/"+token, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: HTTP %d", method, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func fleetToolText(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", out)
	}
	content := result["content"].([]any)
	isError, _ := result["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isError
}

func TestFleetMCPToolsActAsTheCallingSeat(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold), "b": newFakeFleetSeat(fleetAnswer("child reply")), "other": newFakeFleetSeat(fleetHold)}
	r := newFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("other", FleetOwner)
	srv := httptest.NewServer(r.k)
	defer srv.Close()
	r.mu.Lock()
	token, otherToken := r.specs[0].Token, r.specs[1].Token
	r.mu.Unlock()

	if out := fleetRPC(t, srv, token, "initialize", map[string]any{}); out["result"].(map[string]any)["protocolVersion"] != fleetMCPProtocol {
		t.Fatalf("initialize: %+v", out)
	}
	tools := fleetRPC(t, srv, token, "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	if len(names) != 6 || !names["start"] || !names["send"] || !names["stop"] || !names["list"] || !names["cancel"] || !names["check"] {
		t.Fatalf("tools: %v", names)
	}
	call := func(token, name string, args map[string]any) (string, bool) {
		t.Helper()
		return fleetToolText(t, fleetRPC(t, srv, token, "tools/call", map[string]any{"name": name, "arguments": args}))
	}
	if text, isErr := call(token, "start", map[string]any{"name": "b", "instructions": "be a child"}); isErr {
		t.Fatalf("start: %s", text)
	}
	if text, isErr := call(token, "start", map[string]any{"name": "b", "instructions": "again"}); !isErr || !strings.Contains(text, "already exists") {
		t.Fatalf("a bad start must fail at once: %q %v", text, isErr)
	}
	if text, isErr := call(token, "send", map[string]any{"to": "b", "text": "go"}); isErr {
		t.Fatalf("send: %s", text)
	}
	waitFleet(t, "child's reply to reach its requester", func() bool {
		for _, text := range seats["a"].sends() {
			if strings.Contains(text, "child reply") {
				return true
			}
		}
		return false
	})
	if text, isErr := call(otherToken, "stop", map[string]any{"name": "b"}); !isErr || !strings.Contains(text, "may stop only") {
		t.Fatalf("another tree's stop: %q %v", text, isErr)
	}
	text, isErr := call(token, "list", nil)
	var st FleetStatus
	if isErr || json.Unmarshal([]byte(text), &st) != nil || len(st.Agents) != 3 {
		t.Fatalf("list: %q", text)
	}
	if text, isErr := call(token, "bogus", nil); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("bogus: %q %v", text, isErr)
	}
}

func TestFleetMCPRefusesUnknownTokensAndAnswersOnlyPost(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	srv := httptest.NewServer(r.k)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/upstream/fleet/nope", "application/json", strings.NewReader(`{"id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown token: HTTP %d", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/upstream/fleet/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: HTTP %d", resp.StatusCode)
	}
}

func TestFleetMCPNotificationGetsNoResponseBody(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	srv := httptest.NewServer(r.k)
	defer srv.Close()
	r.mu.Lock()
	token := r.specs[0].Token
	r.mu.Unlock()
	resp, err := http.Post(srv.URL+"/upstream/fleet/"+token, "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: HTTP %d", resp.StatusCode)
	}
}

func TestFleetSeatThatDiesComesBackAndFinishesTheJob(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	id := r.send(FleetOwner, "a", "the job")
	waitFleet(t, "turn", func() bool { return len(seat.sends()) == 1 })

	next := newFakeFleetSeat(fleetAnswer("done anyway"))
	r.setSeat("a", next)
	seat.die(errors.New("process exited"))

	var seq uint64
	m := r.ownerMessage(&seq)
	if m.Text != "done anyway" || m.InReplyTo != id {
		t.Fatalf("owner got %+v", m)
	}
	if got := next.sends()[0]; !strings.Contains(got, "the job") || !strings.Contains(got, "BRIEF-a") {
		t.Fatalf("redelivery: %q", got)
	}
	st, err := r.k.Status("f")
	if err != nil || len(st.Agents) != 1 || seat.stops != 0 {
		t.Fatalf("status %+v %v, stops %d", st, err, seat.stops)
	}
}

func TestFleetSeatThatKeepsDyingRetiresAndItsRequesterHears(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{"p": newFakeFleetSeat(fleetAnswer(FleetNoReply))})
	r.start("p", FleetOwner)
	r.setStartFn(func(_ context.Context, spec *FleetSeatSpec) (FleetSeat, error) {
		if spec.Name == "p" {
			return r.seats["p"], nil
		}
		seat := newFakeFleetSeat(fleetHold)
		seat.dieOnSend = errors.New("crash on start")
		return seat, nil
	})
	if err := r.k.Start(context.Background(), "f", "p", &FleetStartArgs{Name: "c", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	r.send("p", "c", "work")
	waitFleet(t, "the requester to hear", func() bool { return fleetSeatHeard(r.seats["p"], "died 3 times") })
	st, err := r.k.Status("f")
	if err != nil || len(st.Agents) != 1 {
		t.Fatalf("status %+v %v", st, err)
	}
}

// Review finding 3: a plan refusal must not destroy the agent and its seat.
func TestFleetFailedTurnKeepsTheAgentAndTellsTheRequester(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	seat.failTurn = errors.New("429 usage limit")
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	id := r.send(FleetOwner, "a", "work")
	var seq uint64
	m := r.ownerMessage(&seq)
	if !strings.Contains(m.Text, "429 usage limit") || m.InReplyTo != id {
		t.Fatalf("owner got %+v", m)
	}
	st, err := r.k.Status("f")
	if err != nil || len(st.Agents) != 1 || seat.stops != 0 {
		t.Fatalf("agent lost: %+v %v, stops %d", st, err, seat.stops)
	}
	seat.failTurn = nil
	if _, err := r.k.Send("f", &FleetSendArgs{From: FleetOwner, To: "a", Kind: FleetNotice, Text: "again"}); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "a second turn", func() bool { return len(seat.sends()) == 2 })
}

// Review finding 4: a passing outage must not lose agents for good.
func TestFleetReattachKeepsTryingAndComesBackWhenItCan(t *testing.T) {
	parent, child := newFakeFleetSeat(fleetAnswer(FleetNoReply)), newFakeFleetSeat(fleetAnswer("back"))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"p": parent, "c": child})
	r.start("p", FleetOwner)
	r.start("c", "p")
	r.send("p", "c", "first")
	waitFleet(t, "the conversation to exist", func() bool { return fleetSeatHeard(parent, "back") })
	r.k.Close()

	var held atomic.Bool
	held.Store(true)
	r.setStartFn(func(_ context.Context, spec *FleetSeatSpec) (FleetSeat, error) {
		if spec.Name == "c" && held.Load() {
			return nil, fmt.Errorf("%w: %s", ErrFleetSeatHeld, spec.Name)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.seats[spec.Name], nil
	})
	r.open()
	waitFleet(t, "the parent to hear once", func() bool { return fleetSeatHeard(parent, "cannot come back yet") })
	before := r.specCount()
	waitFleet(t, "further tries", func() bool { return r.specCount() > before+5 })
	st, err := r.k.Status("f")
	if err != nil || len(st.Agents) != 2 {
		t.Fatalf("a hold lost an agent: %+v %v", st, err)
	}
	held.Store(false)
	waitFleet(t, "the agent to come back", func() bool {
		st, err := r.k.Status("f")
		if err != nil {
			return false
		}
		for _, a := range st.Agents {
			if a.Name == "c" {
				return a.State == FleetStateIdle
			}
		}
		return false
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if last := r.specs[len(r.specs)-1]; last.Name != "c" || !last.Resume {
		t.Fatalf("a policy hold cost the agent its conversation: %+v", last)
	}
	if count := strings.Count(strings.Join(parent.sends(), "|"), "cannot come back yet"); count != 1 {
		t.Fatalf("parent told %d times", count)
	}
}

func TestFleetConversationThatCannotResumeStartsFreshAndIsRebriefed(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("first answer"))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "first")
	var seq uint64
	r.ownerMessage(&seq)
	r.k.Close()

	fresh := newFakeFleetSeat(fleetAnswer("second answer"))
	r.setStartFn(func(_ context.Context, spec *FleetSeatSpec) (FleetSeat, error) {
		if spec.Resume {
			return nil, fmt.Errorf("%w: refused", ErrFleetConversationLost)
		}
		return fresh, nil
	})
	r.open()
	var m *FleetMessage
	for m == nil || !strings.Contains(m.Text, "fresh one") {
		m = r.ownerMessage(&seq)
	}
	r.send(FleetOwner, "a", "second")
	waitFleet(t, "next turn", func() bool { return len(fresh.sends()) == 1 })
	if got := fresh.sends()[0]; !strings.Contains(got, "BRIEF-a") {
		t.Fatalf("no brief on a fresh conversation: %q", got)
	}
}

func TestFleetAgentWithNoConversationYetDoesNotAskToResume(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.k.Close()
	r.open()
	waitFleet(t, "reattach", func() bool { return r.specCount() == 2 })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.specs[1].Resume {
		t.Fatal("asked to resume a conversation that never began")
	}
}

func TestFleetAgentStuckComingBackCanBeStopped(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "work")
	waitFleet(t, "turn", func() bool { return len(r.seats["a"].sends()) == 1 })
	r.k.Close()
	r.setSeat("a", nil)
	r.open()
	if err := r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "a"); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	var m *FleetMessage
	for m == nil || !strings.Contains(m.Text, "agent a is gone") {
		m = r.ownerMessage(&seq)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.discarded) != 1 || r.discarded[0] != "a" {
		t.Fatalf("the seat it left behind was not removed: %v", r.discarded)
	}
}

// Review finding 1: a reused name made a parent cycle, which froze the kernel.
func TestFleetNameReuseCannotInheritOrCycle(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"y": newFakeFleetSeat(fleetHold), "x": newFakeFleetSeat(fleetHold), "z": newFakeFleetSeat(fleetHold)}
	r := newFleetRig(t, seats)
	ctx := wallclockguard.UntilTestTimeout(t)
	r.start("y", FleetOwner)
	r.start("x", "y")
	r.start("z", FleetOwner)
	if err := r.k.StopTree(ctx, "f", "y"); err != nil {
		t.Fatal(err)
	}
	r.start("x", FleetOwner)
	seats["y"] = newFakeFleetSeat(fleetHold)
	r.start("y", "x")
	done := make(chan error, 1)
	go func() { done <- r.k.Stop("f", "z", "x") }()
	if err := <-done; err == nil {
		t.Fatal("an unrelated agent stopped x")
	}

	// An orphan answers to the owner, and not to the next holder of the name.
	seats2 := map[string]*fakeFleetSeat{"p": newFakeFleetSeat(fleetHold), "c": newFakeFleetSeat(fleetHold)}
	r2 := newFleetRig(t, seats2)
	r2.start("p", FleetOwner)
	r2.start("c", "p")
	if err := r2.k.Stop("f", FleetOwner, "p"); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "p to go", func() bool {
		st, err := r2.k.Status("f")
		return err == nil && len(st.Agents) == 1
	})
	st, _ := r2.k.Status("f")
	if st.Agents[0].Parent != FleetOwner {
		t.Fatalf("orphan's parent: %+v", st.Agents[0])
	}
	seats2["p"] = newFakeFleetSeat(fleetHold)
	r2.start("p", FleetOwner)
	if err := r2.k.Stop("f", "p", "c"); err == nil {
		t.Fatal("a new agent inherited authority with its name")
	}
}

// Review finding 5: StopTree must mean the seat is gone.
func TestFleetStopTreeWaitsForTheSeatAndCannotHitASuccessor(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	seat.stopHold = make(chan struct{})
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	done := make(chan error, 1)
	go func() { done <- r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "a") }()
	waitFleet(t, "stop to begin", func() bool {
		seat.mu.Lock()
		defer seat.mu.Unlock()
		return seat.stops > 0
	})
	select {
	case err := <-done:
		t.Fatalf("StopTree returned while the seat was alive: %v", err)
	default:
	}
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"}); err == nil {
		t.Fatal("the name was free while its seat still ran")
	}
	close(seat.stopHold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Review finding 7: a request sent while an agent starts is still owed an answer.
func TestFleetRequestToAnAgentThatFailsToStartIsAnswered(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	entered, release := make(chan struct{}), make(chan struct{})
	r.setStartFn(func(context.Context, *FleetSeatSpec) (FleetSeat, error) {
		close(entered)
		<-release
		return nil, errors.New("no model")
	})
	failed := make(chan error, 1)
	go func() {
		failed <- r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"})
	}()
	<-entered
	id := r.send(FleetOwner, "a", "are you there")
	close(release)
	if err := <-failed; err == nil {
		t.Fatal("start succeeded")
	}
	var seq uint64
	m := r.ownerMessage(&seq)
	if !strings.Contains(m.Text, "could not start") || m.InReplyTo != id {
		t.Fatalf("owner got %+v", m)
	}
}

func TestFleetToolStartReportsALaterFailureToTheCaller(t *testing.T) {
	parent := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"p": parent})
	r.start("p", FleetOwner)
	if err := r.k.startForAgent("f", "p", &FleetStartArgs{Name: "c", Instructions: "x"}); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "the caller to hear", func() bool { return fleetSeatHeard(parent, "could not start") })
}

// Review finding 13: nothing can slip in behind a shutdown.
func TestFleetStartDuringShutdownIsRefused(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	seat.stopHold = make(chan struct{})
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	done := make(chan error, 1)
	go func() { done <- r.k.Shutdown(wallclockguard.UntilTestTimeout(t), "f") }()
	waitFleet(t, "shutdown to begin", func() bool {
		r.k.mu.Lock()
		defer r.k.mu.Unlock()
		return r.k.state.Fleets["f"].Closing
	})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "b", Instructions: "x", WorkDir: "/tmp"}); err == nil {
		t.Fatal("started an agent during shutdown")
	}
	close(seat.stopHold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Review finding 14: a kernel that refused to open must not touch the state.
func TestFleetFailedOpenArmsNoTimers(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	r.start("a", FleetOwner)
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "a", Text: "later", After: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := r.k.Open("g", nil); err != nil {
		t.Fatal(err)
	}
	r.k.Close()
	statePath := filepath.Join(r.dir, "fleet-state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "g.events.jsonl"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "f.events.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := OpenFleetKernel(&FleetKernelArgs{Dir: r.dir, Start: func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return nil, errors.New("no") }})
	if err == nil {
		k.Close()
		t.Fatal("damaged log accepted")
	}
	k2, err := OpenFleetKernel(&FleetKernelArgs{Dir: t.TempDir(), Start: func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return nil, errors.New("no") }})
	if err != nil {
		t.Fatal(err)
	}
	k2.Close()
	if len(k2.timers) != 0 {
		t.Fatal("timers armed before Resume")
	}
	if after, err := os.ReadFile(statePath); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a failed open rewrote the state: %v", err)
	}
}

// Review finding 2: a backlog must reach a returning reader in pieces that fit
// a broker frame, whatever its size.
func TestFleetReadBatchesFitAFrameAndEverythingArrives(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	const messages = 12
	for range messages {
		r.send(FleetOwner, "a", strings.Repeat("\x01", fleetMaxText+10))
	}
	ctx := wallclockguard.UntilTestTimeout(t)
	var seq uint64
	seen := 0
	st, err := r.k.Status("f")
	if err != nil {
		t.Fatal(err)
	}
	for seq < st.LastSeq {
		events, err := r.k.Read(ctx, "f", seq)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) >= 1<<20 {
			t.Fatalf("a read of %d events encodes to %d bytes", len(events), len(raw))
		}
		for i := range events {
			seq = events[i].Seq
			if m := events[i].Message; m != nil {
				seen++
				if !strings.HasSuffix(m.Text, "[truncated by Claudia]") {
					t.Fatal("over-long text was not bounded")
				}
			}
		}
	}
	if seen != messages {
		t.Fatalf("saw %d of %d messages", seen, messages)
	}
}

func TestFleetReadWaitsAndHonoursItsContext(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events, err := r.k.Read(ctx, "f", 0)
	if err != nil || events != nil {
		t.Fatalf("cancelled read: %v %v", events, err)
	}
	got := make(chan []FleetEvent, 1)
	go func() {
		events, _ := r.k.Read(context.Background(), "f", 0)
		got <- events
	}()
	r.start("a", FleetOwner)
	if events := <-got; len(events) == 0 {
		t.Fatal("no events")
	}
}

// Review finding 8: an unrelated server that shares the name is left alone.
func TestLocalMCPServerDoesNotCaptureAnOrdinaryServerOfTheSameName(t *testing.T) {
	h, err := NewMCPHost(&MCPHostArgs{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.ServeLocal(FleetMCPServerName, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the local handler answered an ordinary server")
	}))
	local := h.LocalURL(FleetMCPServerName, "tok")
	got := h.Attach([]MCPServer{{Name: FleetMCPServerName, Type: "http", URL: strings.Replace(local, h.Addr(), "127.0.0.1:1", 1)}})
	if got[0].URL != local {
		t.Fatalf("a seat's URL from an earlier listener became %q, want %q", got[0].URL, local)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	got = h.Attach([]MCPServer{{Name: FleetMCPServerName, Type: "http", URL: upstream.URL + "/mcp"}})
	if strings.Contains(got[0].URL, "/upstream/"+FleetMCPServerName+"/") {
		t.Fatalf("an ordinary server was captured: %q", got[0].URL)
	}
}

// Re-review finding 1: ordinary failures, however many, never cost the agent its
// conversation. Only the provider saying it is gone does.
func TestFleetRepeatedFailuresKeepAskingToResume(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("first answer"))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "first")
	var seq uint64
	r.ownerMessage(&seq)
	r.k.Close()
	r.setStartFn(func(_ context.Context, spec *FleetSeatSpec) (FleetSeat, error) {
		return nil, errors.New("sidecar not listening")
	})
	r.open()
	waitFleet(t, "many tries", func() bool { return r.specCount() > 12 })
	r.k.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, spec := range r.specs[1:] {
		if !spec.Resume {
			t.Fatalf("gave up on the conversation after ordinary failures: %+v", spec)
		}
	}
}

// Re-review finding 6: a stop must not wait out a slow start.
func TestFleetStopCancelsAReviveStartInProgress(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	r.start("a", FleetOwner)
	r.k.Close()
	entered := make(chan struct{}, 1)
	r.setStartFn(func(ctx context.Context, _ *FleetSeatSpec) (FleetSeat, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r.open()
	<-entered
	if err := r.k.StopTree(wallclockguard.UntilTestTimeout(t), "f", "a"); err != nil {
		t.Fatal(err)
	}
	st, err := r.k.Status("f")
	if err != nil || len(st.Agents) != 0 {
		t.Fatalf("status %+v %v", st, err)
	}
}

// Re-review finding 4: no field of an event may be too big to read.
func TestFleetOversizedErrorTextIsBounded(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	seat.failTurn = errors.New(strings.Repeat("e", 2*fleetMaxText))
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "work")
	var seq uint64
	r.ownerMessage(&seq)
	events, err := r.k.Read(wallclockguard.UntilTestTimeout(t), "f", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if len(e.Cause) > fleetMaxText+100 {
			t.Fatalf("event %d carries a %d-byte cause", e.Seq, len(e.Cause))
		}
	}
}

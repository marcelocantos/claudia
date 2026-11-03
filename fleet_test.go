// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fleetTestWait = 5 * time.Second

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
	s.mu.Unlock()
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
	s.once.Do(func() { close(s.gone) })
}
func (s *fakeFleetSeat) Describe() string        { return "fake/model" }
func (s *fakeFleetSeat) Usage() any              { return nil }
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
	k, err := OpenFleetKernel(&FleetKernelArgs{Dir: r.dir, Start: func(ctx context.Context, spec *FleetSeatSpec) (FleetSeat, error) {
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
	id, err := r.k.Send("f", from, to, FleetRequest, text, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// ownerMessage waits for the next message to the owner after *seq.
func (r *fleetRig) ownerMessage(seq *uint64) *FleetMessage {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fleetTestWait)
	defer cancel()
	for {
		events, err := r.k.Read(ctx, "f", *seq)
		if err != nil {
			r.t.Fatal(err)
		}
		if events == nil {
			r.t.Fatal("timed out waiting for a message to the owner")
		}
		for i := range events {
			*seq = events[i].Seq
			if m := events[i].Message; m != nil && m.To == FleetOwner {
				return m
			}
		}
	}
}

func waitFleet(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(fleetTestWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(time.Millisecond)
	}
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
		{"seat dies", func(r *fleetRig) { r.seats["b"].death <- errors.New("process exited") }},
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
	if _, err := r.k.Send("f", FleetOwner, "a", FleetRequest, "later", 20*time.Millisecond); err != nil {
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
	if _, err := r.k.Send("f", FleetOwner, "c", FleetNotice, "quietly", 0); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "silent turn", func() bool { return len(child.sends()) == 1 })
	if _, err := r.k.Send("f", FleetOwner, "c", FleetNotice, "report", 0); err != nil {
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
	if _, err := r.k.Send("f", "a", "a", "", "wake up", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "timer", func() bool { return len(seat.sends()) == 1 })
	if got := seat.sends()[0]; !strings.Contains(got, "[timer") || !strings.Contains(got, "wake up") {
		t.Fatalf("got %q", got)
	}
	if _, err := r.k.Send("f", "a", "a", "", "now", 0); err == nil {
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
	r.startFn = nil
	for name, err := range map[string]error{
		"bad fleet name":     k.Open("Bad Name", nil),
		"unknown fleet":      k.Start(ctx, "nope", FleetOwner, &FleetStartArgs{Name: "a", Instructions: "x", WorkDir: "/tmp"}),
		"bad agent name":     k.Start(ctx, "f", FleetOwner, &FleetStartArgs{Name: "Bad Name", Instructions: "x", WorkDir: "/tmp"}),
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
	if _, err := k.Send("f2", FleetOwner, "a", "", "one", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Send("f2", FleetOwner, "a", "", "two", 0); err == nil || !strings.Contains(err.Error(), "message limit") {
		t.Fatalf("message limit: %v", err)
	}
	if _, err := k.Send("f", FleetOwner, "ghost", "", "x", 0); err == nil {
		t.Fatal("sent to an unknown agent")
	}
	if _, err := k.Send("f", FleetOwner, "a", "response", "x", 0); err == nil {
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
	r.seats["a"] = newFakeFleetSeat(fleetHold)
	r.start("a", FleetOwner)
	ctx, cancel := context.WithTimeout(context.Background(), fleetTestWait)
	defer cancel()
	events, err := r.k.Read(ctx, "f", 0)
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
	r.seats["a"] = next
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
	if _, err := r.k.Send("f", "a", "a", "", "after the restart", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	r.k.Close()

	next := newFakeFleetSeat(fleetAnswer(FleetNoReply))
	r.seats["a"] = next
	r.open()
	waitFleet(t, "timer after restart", func() bool { return len(next.sends()) == 1 })
	if got := next.sends()[0]; !strings.Contains(got, "after the restart") {
		t.Fatalf("got %q", got)
	}
	// A client that was away reads what it missed, and sequence numbers go on.
	ctx, cancel := context.WithTimeout(context.Background(), fleetTestWait)
	defer cancel()
	events, err := r.k.Read(ctx, "f", 0)
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

func TestFleetAgentThatCannotReattachIsReportedToItsParent(t *testing.T) {
	parent, child := newFakeFleetSeat(fleetAnswer("noted")), newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"p": parent, "c": child})
	r.start("p", FleetOwner)
	r.start("c", "p")
	r.k.Close()
	r.seats["c"] = nil
	r.open()
	waitFleet(t, "parent to hear", func() bool {
		for _, text := range parent.sends() {
			if strings.Contains(text, "could not reattach") {
				return true
			}
		}
		return false
	})
}

func TestFleetStartInterruptedByCloseIsReattached(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	entered := make(chan struct{})
	r.startFn = func(ctx context.Context, _ *FleetSeatSpec) (FleetSeat, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
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
	r.seats["a"] = seat
	r.startFn = nil
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

func TestFleetReadWaitsAndHonoursItsContext(t *testing.T) {
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetHold)})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	events, err := r.k.Read(ctx, "f", 0)
	if err != nil || events != nil {
		t.Fatalf("idle read: %v %v", events, err)
	}
	got := make(chan []FleetEvent, 1)
	go func() {
		events, _ := r.k.Read(context.Background(), "f", 0)
		got <- events
	}()
	r.start("a", FleetOwner)
	select {
	case events := <-got:
		if len(events) == 0 {
			t.Fatal("no events")
		}
	case <-time.After(fleetTestWait):
		t.Fatal("reader was not woken")
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

func TestFleetLargeTextIsBounded(t *testing.T) {
	seat := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"a": seat})
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", strings.Repeat("x", fleetMaxText+10))
	waitFleet(t, "turn", func() bool { return len(seat.sends()) == 1 })
	if got := seat.sends()[0]; !strings.Contains(got, "[truncated by Claudia]") || len(got) > fleetMaxText+1000 {
		t.Fatalf("delivery of %d bytes", len(got))
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
	if len(names) != 4 || !names["start"] || !names["send"] || !names["stop"] || !names["list"] {
		t.Fatalf("tools: %v", names)
	}
	call := func(token, name string, args map[string]any) (string, bool) {
		t.Helper()
		return fleetToolText(t, fleetRPC(t, srv, token, "tools/call", map[string]any{"name": name, "arguments": args}))
	}
	if text, isErr := call(token, "start", map[string]any{"name": "b", "instructions": "be a child"}); isErr {
		t.Fatalf("start: %s", text)
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

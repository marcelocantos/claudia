// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 🎯T250: expectations wake a sender once; expediting steers or interrupts.

// clockNotices are the notices from the clock an agent's seat was given.
func clockNotices(seat *fakeFleetSeat) []string {
	var out []string
	for _, text := range seat.sends() {
		if strings.Contains(text, "[notice from "+FleetClockSender+"]") {
			out = append(out, text)
		}
	}
	return out
}

func (r *fleetRig) expects() int {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	return len(r.k.state.Fleets["f"].Expects)
}

func (r *fleetRig) inbox(name string) []string {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	var ids []string
	for _, m := range r.k.state.Fleets["f"].Agents[name].Inbox {
		ids = append(ids, m.ID)
	}
	return ids
}

func (r *fleetRig) working(name string) bool {
	r.k.mu.Lock()
	defer r.k.mu.Unlock()
	a := r.k.state.Fleets["f"].Agents[name]
	return a != nil && a.Current != nil
}

func TestFleetMissedAcceptWakesTheSenderOnceSayingWhereItStands(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetHold)}
	r, clock := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	busy := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	id, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "b", Text: "quick one", AcceptWithin: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n := len(clockNotices(seats["a"])); n != 0 {
		t.Fatal("woke the sender early")
	}
	advanceUntil(t, clock, time.Minute, "the clock's notice", func() bool { return len(clockNotices(seats["a"])) == 1 })
	got := clockNotices(seats["a"])[0]
	for _, want := range []string{id + " to b has not been taken up 2m", "number 1 in b's queue", "on " + busy + " from owner, has run 2m", "expedite"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice lacks %q: %q", want, got)
		}
	}
	clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if n := len(clockNotices(seats["a"])); n != 1 || r.expects() != 0 {
		t.Fatalf("woke %d times; %d expectations left", n, r.expects())
	}
}

func TestFleetMetExpectationsAreSilent(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetAnswer("done"))}
	r, clock := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "b", Text: "go", AcceptWithin: time.Minute, AnswerWithin: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "the answer", func() bool { return fleetSeatHeard(seats["a"], "done") })
	clock.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	if n := len(clockNotices(seats["a"])); n != 0 || r.expects() != 0 {
		t.Fatalf("met expectations woke the sender %d times; %d left", n, r.expects())
	}
}

func TestFleetMissedAnswerSaysTheRequestWasTakenUp(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetHold)}
	r, clock := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	id, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "b", Text: "go", AcceptWithin: time.Minute, AnswerWithin: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "b to take it up", func() bool { return r.working("b") })
	advanceUntil(t, clock, time.Minute, "the clock's notice", func() bool { return len(clockNotices(seats["a"])) == 1 })
	got := clockNotices(seats["a"])[0]
	for _, want := range []string{id + " to b has no answer 10m", "b took it up 10m ago and is still working on it"} {
		if !strings.Contains(got, want) {
			t.Fatalf("notice lacks %q: %q", want, got)
		}
	}
}

func TestFleetExpectationsSurviveARestartAndDieWithTheirSender(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetHold), "c": newFakeFleetSeat(fleetAnswer(FleetNoReply))}
	r, clock := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	r.start("c", FleetOwner)
	r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	if _, err := r.k.Send("f", &FleetSendArgs{From: "a", To: "b", Text: "x", AcceptWithin: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.k.Send("f", &FleetSendArgs{From: "c", To: "b", Text: "y", AcceptWithin: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	r.k.Close()
	r.open()
	if err := r.k.Stop("f", FleetOwner, "c"); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "c to go", func() bool { return r.expects() == 1 })
	advanceUntil(t, clock, time.Minute, "the clock's notice after a restart", func() bool { return len(clockNotices(seats["a"])) == 1 })
}

func TestFleetSendRefusesExpectationsThatCannotApply(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetAnswer(FleetNoReply))}
	r, _ := newClockedFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	for name, args := range map[string]*FleetSendArgs{
		"scheduled":        {From: "a", To: "b", Text: "x", After: time.Minute, AcceptWithin: time.Minute},
		"to the owner":     {From: "a", To: FleetOwner, Text: "x", AnswerWithin: time.Minute},
		"answer to notice": {From: FleetOwner, To: "b", Kind: FleetNotice, Text: "x", AnswerWithin: time.Minute},
		"negative":         {From: "a", To: "b", Text: "x", AcceptWithin: -time.Minute},
		"bad mode":         {From: FleetOwner, To: "b", Text: "x", Mode: "shout"},
		"no authority":     {From: "a", To: "b", Text: "x", Mode: FleetExpediteSteer},
	} {
		if _, err := r.k.Send("f", args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := r.k.Start(t.Context(), "f", FleetOwner, &FleetStartArgs{Name: FleetClockSender, Instructions: "x", WorkDir: "/tmp"}); err == nil {
		t.Error("an agent took the clock's name")
	}
}

func TestFleetSteerFoldsARequestIntoTheTurnWhoseReplyAnswersBoth(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"b": b})
	r.start("b", FleetOwner)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	id, note, err := r.k.Deliver("f", &FleetSendArgs{From: FleetOwner, To: "b", Text: "also this", Mode: FleetExpediteSteer, AcceptWithin: time.Minute})
	if err != nil || !strings.Contains(note, "steered "+id) {
		t.Fatalf("deliver: %q %q %v", id, note, err)
	}
	if got := b.steers(); len(got) != 1 || !strings.Contains(got[0], fleetSteeredNote) || !strings.Contains(got[0], "also this") {
		t.Fatalf("steered %q", got)
	}
	if r.expects() != 0 {
		t.Fatal("steering did not meet the accept expectation")
	}
	b.replies <- "both done"
	var seq uint64
	answered := map[string]bool{}
	for len(answered) < 2 {
		m := r.ownerMessage(&seq)
		if m.Text != "both done" {
			t.Fatalf("owner heard %+v", m)
		}
		answered[m.InReplyTo] = true
	}
	if !answered[first] || !answered[id] {
		t.Fatalf("answered %v", answered)
	}
}

func TestFleetSteerASeatCannotDoPutsTheMessageFirst(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	b.steerErr = ErrSteerUnsupported
	r := newFleetRig(t, map[string]*fakeFleetSeat{"b": b})
	r.start("b", FleetOwner)
	r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	behind := r.send(FleetOwner, "b", "waiting")
	urgent := r.send(FleetOwner, "b", "urgent")
	note, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteSteer)
	if err != nil || !strings.Contains(note, "could not steer") {
		t.Fatalf("expedite: %q %v", note, err)
	}
	if got := r.inbox("b"); len(got) != 2 || got[0] != urgent || got[1] != behind {
		t.Fatalf("queue %v", got)
	}
}

func TestFleetInterruptAnswersTheInterruptedRequestAndTakesTheMessageNext(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	b.interruptReply = "half done"
	r := newFleetRig(t, map[string]*fakeFleetSeat{"b": b})
	r.start("b", FleetOwner)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	r.send(FleetOwner, "b", "waiting")
	id, note, err := r.k.Deliver("f", &FleetSendArgs{From: FleetOwner, To: "b", Text: "stop and do this", Mode: FleetExpediteInterrupt})
	if err != nil || !strings.Contains(note, "stop its turn") {
		t.Fatalf("deliver: %q %v", note, err)
	}
	var seq uint64
	m := r.ownerMessage(&seq)
	if m.InReplyTo != first || !strings.Contains(m.Text, "owner interrupted this turn to take up "+id) || !strings.Contains(m.Text, "half done") {
		t.Fatalf("interrupted requester heard %+v", m)
	}
	waitFleet(t, "the urgent message next", func() bool {
		sent := b.sends()
		return len(sent) == 2 && strings.Contains(sent[1], "stop and do this")
	})
}

func TestFleetExpediteTakesAuthorityOverTheRecipient(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetHold)}
	r := newFleetRig(t, seats)
	r.start("a", FleetOwner)
	r.start("b", FleetOwner)
	r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	id := r.send("a", "b", "mine")
	if _, err := r.k.Expedite("f", "a", id, FleetExpediteInterrupt); err == nil || !strings.Contains(err.Error(), "no authority") {
		t.Fatalf("a expedited into b: %v", err)
	}
	if _, err := r.k.Expedite("f", FleetOwner, "r999", FleetExpediteSteer); !errors.Is(err, errFleetNotWaiting) {
		t.Fatalf("unknown message: %v", err)
	}
}

func TestFleetSteeredMessagesSurviveARestart(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"b": b})
	r.start("b", FleetOwner)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	steered := r.send(FleetOwner, "b", "also this")
	if _, err := r.k.Expedite("f", FleetOwner, steered, FleetExpediteSteer); err != nil {
		t.Fatal(err)
	}
	r.k.Close()
	b.reply = fleetAnswer("redone")
	r.open()
	var seq uint64
	answered := map[string]bool{}
	for len(answered) < 2 {
		m := r.ownerMessage(&seq)
		if m.Text == "redone" {
			answered[m.InReplyTo] = true
		}
	}
	if !answered[first] || !answered[steered] {
		t.Fatalf("answered %v", answered)
	}
}

func TestFleetMCPSendTakesExpectationsAndExpediteIsATool(t *testing.T) {
	seats := map[string]*fakeFleetSeat{"a": newFakeFleetSeat(fleetAnswer(FleetNoReply)), "b": newFakeFleetSeat(fleetHold)}
	r := newFleetRig(t, seats)
	r.start("a", FleetOwner)
	srv := httptest.NewServer(r.k)
	defer srv.Close()
	r.mu.Lock()
	token := r.specs[0].Token
	r.mu.Unlock()
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		return fleetToolText(t, fleetRPC(t, srv, token, "tools/call", map[string]any{"name": name, "arguments": args}))
	}
	if text, isErr := call("start", map[string]any{"name": "b", "instructions": "child"}); isErr {
		t.Fatal(text)
	}
	waitFleet(t, "b up", func() bool {
		st, _ := r.k.Status("f")
		return len(st.Agents) == 2 && st.Agents[1].State != FleetStateStarting
	})
	if text, isErr := call("send", map[string]any{"to": "b", "text": "long job"}); isErr {
		t.Fatal(text)
	}
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	text, isErr := call("send", map[string]any{"to": "b", "text": "next", "accept_within_seconds": 60, "answer_within_seconds": 600, "mode": "steer"})
	if isErr || !strings.Contains(text, "steered") {
		t.Fatalf("send: %q", text)
	}
	queued, _ := call("send", map[string]any{"to": "b", "text": "later"})
	id := strings.TrimPrefix(queued, "queued as ")
	if text, isErr := call("expedite", map[string]any{"id": id, "mode": "steer"}); isErr || !strings.Contains(text, "steered "+id) {
		t.Fatalf("expedite: %q", text)
	}
}

// gatedSeat holds Steer or Interrupt until released, to open the windows in
// which the kernel has let go of its lock.
type gatedSeat struct {
	*fakeFleetSeat
	entered chan struct{}
	release chan error
}

func newGatedSeat(seat *fakeFleetSeat) *gatedSeat {
	return &gatedSeat{fakeFleetSeat: seat, entered: make(chan struct{}, 1), release: make(chan error)}
}

func (g *gatedSeat) Steer(string) error { g.entered <- struct{}{}; return <-g.release }
func (g *gatedSeat) Interrupt() error   { g.entered <- struct{}{}; return <-g.release }

func (r *fleetRig) startGated(name string, g *gatedSeat) {
	r.t.Helper()
	r.setStartFn(func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return g, nil })
	r.start(name, FleetOwner)
	r.setStartFn(nil)
}

func TestFleetASteerFailingWhileItsAgentStopsIsStillAnswered(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	b.stopHold = make(chan struct{})
	g := newGatedSeat(b)
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	r.startGated("b", g)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	steered := r.send(FleetOwner, "b", "also this")
	done := make(chan string)
	go func() { note, _ := r.k.Expedite("f", FleetOwner, steered, FleetExpediteSteer); done <- note }()
	<-g.entered
	if err := r.k.Stop("f", FleetOwner, "b"); err != nil {
		t.Fatal(err)
	}
	g.release <- errors.New("seat going away")
	<-done
	close(b.stopHold)
	var seq uint64
	answered := map[string]bool{}
	for len(answered) < 2 {
		answered[r.ownerMessage(&seq).InReplyTo] = true
	}
	if !answered[first] || !answered[steered] {
		t.Fatalf("answered %v", answered)
	}
}

func TestFleetAnInterruptNeverLandsOnTheNextTurn(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	g := newGatedSeat(b)
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	r.startGated("b", g)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	urgent := r.send(FleetOwner, "b", "urgent")
	done := make(chan string)
	go func() { note, _ := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt); done <- note }()
	<-g.entered
	// The turn finishes on its own while the interrupt is still on its way.
	b.replies <- "finished anyway"
	time.Sleep(20 * time.Millisecond)
	if n := len(b.sends()); n != 1 {
		t.Fatalf("started the next turn with an interrupt in flight: %d sends", n)
	}
	g.release <- nil
	<-done
	var seq uint64
	m := r.ownerMessage(&seq)
	if m.InReplyTo != first || !strings.Contains(m.Text, "finished anyway") {
		t.Fatalf("first requester heard %+v", m)
	}
	waitFleet(t, "the urgent message", func() bool { return len(b.sends()) == 2 })
}

func TestFleetSteeringRespectsASpentBudget(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	r := newFleetRig(t, map[string]*fakeFleetSeat{"b": b})
	if err := r.k.Start(context.Background(), "f", FleetOwner, &FleetStartArgs{Name: "b", Instructions: "x", WorkDir: "/tmp", Budget: &FleetBudget{Turns: 1}}); err != nil {
		t.Fatal(err)
	}
	r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	extra := r.send(FleetOwner, "b", "more")
	note, err := r.k.Expedite("f", FleetOwner, extra, FleetExpediteSteer)
	if err != nil || !strings.Contains(note, "budget") || len(b.steers()) != 0 {
		t.Fatalf("steered past a spent budget: %q %v %q", note, err, b.steers())
	}
}

func TestFleetAStopIsHonouredWhileAnInterruptHangs(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	g := newGatedSeat(b)
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	r.startGated("b", g)
	r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	urgent := r.send(FleetOwner, "b", "urgent")
	go func() { _, _ = r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt) }()
	<-g.entered
	defer func() { g.release <- nil }()
	if err := r.k.Stop("f", FleetOwner, "b"); err != nil {
		t.Fatal(err)
	}
	waitFleet(t, "b to retire", func() bool { st, _ := r.k.Status("f"); return len(st.Agents) == 0 })
}

// sendGatedSeat holds Send, as a provider does until its turn has begun.
type sendGatedSeat struct {
	*fakeFleetSeat
	inSend   chan struct{}
	sendGate chan struct{}
}

func (s *sendGatedSeat) Send(text string) error {
	s.inSend <- struct{}{}
	<-s.sendGate
	return s.fakeFleetSeat.Send(text)
}

func TestFleetAnInterruptBeforeTheTurnHasBegunWaitsForIt(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	b.interruptReply = "barely started"
	s := &sendGatedSeat{fakeFleetSeat: b, inSend: make(chan struct{}, 4), sendGate: make(chan struct{}, 4)}
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	r.setStartFn(func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return s, nil })
	r.start("b", FleetOwner)
	r.setStartFn(nil)
	first := r.send(FleetOwner, "b", "job")
	<-s.inSend
	urgent := r.send(FleetOwner, "b", "urgent")
	note, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt)
	if err != nil || !strings.Contains(note, "just starting") {
		t.Fatalf("expedite: %q %v", note, err)
	}
	b.mu.Lock()
	early := b.interrupts
	b.mu.Unlock()
	if early != 0 {
		t.Fatal("interrupted a turn the seat did not have yet")
	}
	s.sendGate <- struct{}{}
	var seq uint64
	m := r.ownerMessage(&seq)
	if m.InReplyTo != first || !strings.Contains(m.Text, "barely started") || !strings.Contains(m.Text, "interrupted this turn") {
		t.Fatalf("first requester heard %+v", m)
	}
	<-s.inSend
	s.sendGate <- struct{}{}
	waitFleet(t, "the urgent message", func() bool { return fleetSeatHeard(b, "urgent") })
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.interrupts != 1 {
		t.Fatalf("%d interrupts", b.interrupts)
	}
}

func TestFleetAFailedInterruptDoesNotEraseADeliveredOne(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	g := newGatedSeat(b)
	g.entered = make(chan struct{}, 2)
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	r.startGated("b", g)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	u1 := r.send(FleetOwner, "b", "urgent1")
	u2 := r.send(FleetOwner, "b", "urgent2")
	go func() { _, _ = r.k.Expedite("f", FleetOwner, u1, FleetExpediteInterrupt) }()
	<-g.entered
	go func() { _, _ = r.k.Expedite("f", FleetOwner, u2, FleetExpediteInterrupt) }()
	<-g.entered
	g.release <- nil
	b.replies <- "half-wri"
	g.release <- errors.New("refused")
	var seq uint64
	if m := r.ownerMessage(&seq); m.InReplyTo != first || !strings.Contains(m.Text, "interrupted this turn") {
		t.Fatalf("a cut-short turn was delivered unmarked: %q", m.Text)
	}
}

// A provider may read steered text only after a long step, so an interrupt
// must still be able to hurry a message already steered in.
func TestFleetInterruptTakesBackASteeredMessage(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	b.interruptReply = "partway"
	r := newFleetRig(t, map[string]*fakeFleetSeat{"b": b})
	r.start("b", FleetOwner)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	urgent := r.send(FleetOwner, "b", "urgent")
	if note, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteSteer); err != nil || !strings.Contains(note, "steered") {
		t.Fatalf("steer: %q %v", note, err)
	}
	if note, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt); err != nil || !strings.Contains(note, "stop its turn") {
		t.Fatalf("interrupt: %q %v", note, err)
	}
	var seq uint64
	m := r.ownerMessage(&seq)
	if m.InReplyTo != first || !strings.Contains(m.Text, "partway") {
		t.Fatalf("first requester heard %+v", m)
	}
	waitFleet(t, "the urgent message as its own turn", func() bool {
		sent := b.sends()
		return len(sent) == 2 && strings.Contains(sent[1], "urgent")
	})
	if !strings.Contains(b.sends()[1], fleetReclaimedNote) {
		t.Fatalf("the rerun does not say it was steered in before: %q", b.sends()[1])
	}
	b.replies <- "PING"
	if m := r.ownerMessage(&seq); m.InReplyTo != urgent || m.Text != "PING" {
		t.Fatalf("urgent requester heard %+v", m)
	}
}

func TestFleetAFailedInterruptLeavesASteeredMessageInItsTurn(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	b.interruptErr = errors.New("refused")
	r := newFleetRig(t, map[string]*fakeFleetSeat{"b": b})
	r.start("b", FleetOwner)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "b's long turn", func() bool { return r.working("b") })
	urgent := r.send(FleetOwner, "b", "urgent")
	if _, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteSteer); err != nil {
		t.Fatal(err)
	}
	note, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt)
	if err != nil || !strings.Contains(note, "stays steered") || len(r.inbox("b")) != 0 {
		t.Fatalf("interrupt: %q %v, queue %v", note, err, r.inbox("b"))
	}
	b.replies <- "both"
	var seq uint64
	answered := map[string]bool{}
	for len(answered) < 2 {
		m := r.ownerMessage(&seq)
		if m.Text != "both" {
			t.Fatalf("owner heard %+v", m)
		}
		answered[m.InReplyTo] = true
	}
	if !answered[first] || !answered[urgent] || len(b.sends()) != 1 {
		t.Fatalf("answered %v after %d sends", answered, len(b.sends()))
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
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
	if err != nil || !strings.Contains(note, "interrupted") {
		t.Fatalf("deliver: %q %v", note, err)
	}
	var seq uint64
	m := r.ownerMessage(&seq)
	if m.InReplyTo != first || !strings.Contains(m.Text, "interrupted by owner, to take up "+id) || !strings.Contains(m.Text, "half done") {
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
	waitFleet(t, "b up", func() bool { st, _ := r.k.Status("f"); return len(st.Agents) == 2 && st.Agents[1].State != FleetStateStarting })
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

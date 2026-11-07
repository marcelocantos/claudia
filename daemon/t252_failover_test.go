// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// 🎯T252. Pimp, 2026-11-07: a seat on SuperGrok ran out of credits and every
// later send went to the same seat and was refused (HTTP 403
// personal-team-blocked:spending-limit). These tests drive the product
// daemon with stub seats: a seat whose provider Claudia picked moves, keeping
// its session, and continues; a seat whose consumer named the provider does
// not.

// grokSpendWall is the refused turn as the daemon-side Agent publishes it
// once the refusal is typed (provider_cause.go, TestOMPGrokSpendWallIsSpendWall).
func grokSpendWall() claudia.Event {
	return claudia.Event{
		Type: "assistant", IsError: true, StopReason: "end_turn",
		Reason: string(claudia.CauseSpendWall),
		Text:   "provider refused the turn: 403 You have run out of credits or need a Grok subscription. (type=personal-team-blocked:spending-limit)",
	}
}

// switchSeat is a fixture seat that can switch provider in place, as every
// sidecar seat can.
type switchSeat struct {
	*seat
	mu       sync.Mutex
	switches []claudia.Provider
	// hold, when set, makes the next switch wait for it to close and then
	// fail, as a sidecar that cannot load the destination does.
	hold chan struct{}
}

func (s *switchSeat) switched() []claudia.Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]claudia.Provider(nil), s.switches...)
}

// failoverFixture boots a daemon whose seats switch, on a fleet snapshot of
// cursor 10%, grok 90%, claude 40%, codex 20%: pick remaining chooses grok,
// and a move off grok chooses claude, then codex.
func failoverFixture(t *testing.T) (*fixture, func() []*switchSeat) {
	t.Helper()
	f := newFixture(t)
	var mu sync.Mutex
	var seats []*switchSeat
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		s := &switchSeat{seat: newSeat()}
		f.mu.Lock()
		f.seats = append(f.seats, s.seat)
		f.mu.Unlock()
		mu.Lock()
		seats = append(seats, s)
		mu.Unlock()
		ops := s.ops()
		ops.SwitchModel = func(p claudia.Provider, model string, _ claudia.SwitchOptions) error {
			s.mu.Lock()
			hold := s.hold
			s.mu.Unlock()
			if hold != nil {
				<-hold
				return errors.New("destination did not load")
			}
			s.mu.Lock()
			s.switches = append(s.switches, claudia.PlanProvider(p))
			s.mu.Unlock()
			return nil
		}
		return claudia.StartStub(ctx, cfg, ops)
	}
	f.boot(t, fleetSnap(t, 10, 90, 40, 20))
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })
	return f, func() []*switchSeat {
		mu.Lock()
		defer mu.Unlock()
		return append([]*switchSeat(nil), seats...)
	}
}

// wireTap owns one raw connection: it answers calls by id and keeps every
// agent event pushed to it.
type wireTap struct {
	t       *testing.T
	c       *broker.Conn
	answers chan *broker.Response
	mu      sync.Mutex
	events  []claudia.Event
}

func newWireTap(t *testing.T, sock string) *wireTap {
	t.Helper()
	c, err := broker.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	w := &wireTap{t: t, c: c, answers: make(chan *broker.Response, 16)}
	go func() {
		for {
			resp, err := c.ReadResponse()
			if err != nil {
				close(w.answers)
				return
			}
			if resp.Type == broker.TypeAgentEvent && resp.AgentEvent != nil {
				ev, err := claudia.DecodeEventWire(resp.AgentEvent.Event)
				if err == nil {
					w.mu.Lock()
					w.events = append(w.events, ev)
					w.mu.Unlock()
				}
				continue
			}
			if resp.ID != "" {
				w.answers <- resp
			}
		}
	}()
	return w
}

func (w *wireTap) call(req *broker.Request) *broker.Response {
	w.t.Helper()
	if err := w.c.WriteRequest(req); err != nil {
		w.t.Fatal(err)
	}
	for resp := range w.answers {
		if resp.ID == req.ID {
			return resp
		}
	}
	w.t.Fatalf("connection closed waiting for %s", req.ID)
	return nil
}

func (w *wireTap) seen() []claudia.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]claudia.Event(nil), w.events...)
}

func (w *wireTap) grant(name string, def claudia.AgentDef, pick string) *broker.GrantResponse {
	w.t.Helper()
	def.Name = name
	if def.WorkDir == "" {
		def.WorkDir = w.t.TempDir()
	}
	if def.SessionID == "" {
		def.SessionID = "sid-" + name
	}
	def.TermLogPath = "-"
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: def})
	if err != nil {
		w.t.Fatal(err)
	}
	resp := w.call(&broker.Request{ID: "grant-" + name, Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Pick: pick}})
	if resp.Type != broker.TypeGranted {
		w.t.Fatalf("grant answered %s: %+v", resp.Type, resp.Error)
	}
	return resp.Granted
}

func (w *wireTap) submit(id, name, text, requestID string) {
	w.t.Helper()
	resp := w.call(&broker.Request{ID: id, Type: broker.TypeSend,
		Send: &broker.SendRequest{Name: name, Text: text, Mode: broker.SendModeSubmit, RequestID: requestID}})
	if resp.Type != broker.TypeSent {
		w.t.Fatalf("send answered %s: %+v", resp.Type, resp.Error)
	}
}

// refuse ends the seat's open turn with an exhaustion refusal, as the
// sidecar does: the turn closes, then the answer is published.
func refuse(t *testing.T, f *fixture, name string, s *switchSeat) {
	t.Helper()
	f.d.mu.Lock()
	proc := f.d.grants[name].proc
	f.d.mu.Unlock()
	s.inFlight.Store(false)
	proc.PublishEvent(grokSpendWall())
}

func progress(evs []claudia.Event, kind string) []claudia.Event {
	var out []claudia.Event
	for _, ev := range evs {
		if ev.ProgressType == kind {
			out = append(out, ev)
		}
	}
	return out
}

func refusalsSeen(evs []claudia.Event) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == "assistant" && ev.IsError {
			n++
		}
	}
	return n
}

func TestT252PickedSeatMovesOffExhaustedProviderAndContinues(t *testing.T) {
	f, seats := failoverFixture(t)
	w := newWireTap(t, f.sock)
	const name = "pimp-ocr-review"
	g := w.grant(name, claudia.AgentDef{}, claudia.PickRemaining)
	if g.Provider != broker.Provider(claudia.ProviderGrok) {
		t.Fatalf("pick chose %s, want grok", g.Provider)
	}
	if def := f.d.reg.Def(name); def == nil || def.ProviderPick != claudia.PickRemaining {
		t.Fatalf("delegation not recorded: %+v", def)
	}
	w.submit("s1", name, "review the OCR fix", "")
	s := seats()[0]
	refuse(t, f, name, s)

	waitFor(t, "continuation submitted", func() bool { return len(s.sent()) == 2 })
	if got := s.switched(); len(got) != 1 || got[0] != claudia.ProviderClaude {
		t.Fatalf("switches = %v, want [claude] (fullest after grok is excluded)", got)
	}
	sent := s.sent()
	if sent[0] != "review the OCR fix" {
		t.Fatalf("first send = %q", sent[0])
	}
	if !strings.Contains(sent[1], "from grok to claude") || strings.Contains(sent[1], "review the OCR fix") {
		t.Fatalf("continuation = %q; want a move note that does not resend the prompt", sent[1])
	}
	def := f.d.reg.Def(name)
	if def == nil || claudia.PlanProvider(def.Provider) != claudia.ProviderClaude ||
		def.SessionID != "sid-"+name || def.ProviderPick != claudia.PickRemaining {
		t.Fatalf("seat after move = %+v; want claude, same session, still delegated", def)
	}
	if f.seatCount() != 1 {
		t.Fatalf("seats started = %d; the move must be in place, not a new seat", f.seatCount())
	}

	waitFor(t, "move announced", func() bool {
		return len(progress(w.seen(), ProgressProviderFailover)) == 1 &&
			len(progress(w.seen(), claudia.ProgressModelSwitch)) == 1
	})
	evs := w.seen()
	if n := refusalsSeen(evs); n != 0 {
		t.Fatalf("consumer saw %d terminal refusals; a move must replace the refusal: %+v", n, evs)
	}
	move := progress(evs, ProgressProviderFailover)[0]
	if move.IsError || move.FromProvider != claudia.ProviderGrok || move.ToProvider != claudia.ProviderClaude ||
		move.Reason != string(claudia.CauseSpendWall) {
		t.Fatalf("failover event = %+v", move)
	}
}

func TestT252PinnedSeatKeepsProviderAndGetsTheRefusal(t *testing.T) {
	f, seats := failoverFixture(t)
	w := newWireTap(t, f.sock)
	const name = "pinned-grok"
	// A consumer that names the provider keeps the choice, even when its
	// definition claims a delegation it was not granted.
	w.grant(name, claudia.AgentDef{Provider: claudia.ProviderGrok, ProviderPick: claudia.PickRemaining}, "")
	if def := f.d.reg.Def(name); def == nil || def.ProviderPick != "" {
		t.Fatalf("a definition claimed its own delegation: %+v", def)
	}
	w.submit("s1", name, "review the OCR fix", "")
	s := seats()[0]
	refuse(t, f, name, s)

	waitFor(t, "refusal forwarded", func() bool { return refusalsSeen(w.seen()) == 1 })
	if got := s.switched(); len(got) != 0 {
		t.Fatalf("a pinned seat was moved: %v", got)
	}
	if got := s.sent(); len(got) != 1 {
		t.Fatalf("sends = %q; a pinned seat gets no continuation", got)
	}
	var refusal claudia.Event
	for _, ev := range w.seen() {
		if ev.Type == "assistant" && ev.IsError {
			refusal = ev
		}
	}
	if refusal.Reason != string(claudia.CauseSpendWall) {
		t.Fatalf("forwarded refusal %+v; want it typed spend_wall", refusal)
	}
}

func TestT252HostRequestIDIsNeverMoved(t *testing.T) {
	f, seats := failoverFixture(t)
	w := newWireTap(t, f.sock)
	const name = "host-correlated"
	w.grant(name, claudia.AgentDef{}, claudia.PickRemaining)
	w.submit("s1", name, "review the OCR fix", "host-req-1")
	s := seats()[0]
	refuse(t, f, name, s)

	waitFor(t, "refusal forwarded", func() bool { return refusalsSeen(w.seen()) == 1 })
	if got := s.switched(); len(got) != 0 {
		t.Fatalf("a send with a host RequestID was moved: %v", got)
	}
}

func TestT252FailoverIsBoundedThenTheRefusalStands(t *testing.T) {
	f, seats := failoverFixture(t)
	w := newWireTap(t, f.sock)
	const name = "bounded"
	w.grant(name, claudia.AgentDef{}, claudia.PickRemaining)
	w.submit("s1", name, "review the OCR fix", "")
	s := seats()[0]

	for hop := 1; hop <= maxExhaustionFailovers; hop++ {
		refuse(t, f, name, s)
		waitFor(t, "continuation", func() bool { return len(s.sent()) == hop+1 })
	}
	// grok refused, then claude; the second move excludes both.
	if got := s.switched(); len(got) != 2 || got[0] != claudia.ProviderClaude || got[1] != claudia.ProviderCodex {
		t.Fatalf("switches = %v, want [claude codex]", got)
	}
	refuse(t, f, name, s)
	waitFor(t, "refusal forwarded", func() bool { return refusalsSeen(w.seen()) == 1 })
	if got := s.switched(); len(got) != maxExhaustionFailovers {
		t.Fatalf("moved past the bound: %v", got)
	}
	notes := progress(w.seen(), ProgressProviderFailover)
	if last := notes[len(notes)-1]; !strings.Contains(last.Text, "stopped") || last.IsError {
		t.Fatalf("last failover note = %+v; want a non-error note saying it stopped", last)
	}

	// A new host submit is a new budget.
	w.submit("s2", name, "next task", "")
	refuse(t, f, name, s)
	waitFor(t, "a new prompt moves again", func() bool { return len(s.switched()) == maxExhaustionFailovers+1 })
}

func TestT252NoEligibleDestinationForwardsTheRefusal(t *testing.T) {
	f, seats := failoverFixture(t)
	w := newWireTap(t, f.sock)
	const name = "grok-only"
	w.grant(name, claudia.AgentDef{AllowedProviders: []claudia.Provider{claudia.ProviderGrok}}, claudia.PickRemaining)
	w.submit("s1", name, "review the OCR fix", "")
	s := seats()[0]
	refuse(t, f, name, s)

	waitFor(t, "refusal forwarded", func() bool { return refusalsSeen(w.seen()) == 1 })
	if got := s.switched(); len(got) != 0 {
		t.Fatalf("moved outside AllowedProviders: %v", got)
	}
	notes := progress(w.seen(), ProgressProviderFailover)
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "not possible") {
		t.Fatalf("failover notes = %+v; want one saying why the refusal stands", notes)
	}
}

// `claudia broker send` reclaims a seat by restating the definition the
// grants list shows, with no pick. That must keep the delegation.
func TestT252ReclaimThatRestatesTheProviderKeepsTheDelegation(t *testing.T) {
	f, seats := failoverFixture(t)
	const name = "reclaimed"
	first := newWireTap(t, f.sock)
	first.grant(name, claudia.AgentDef{}, claudia.PickRemaining)
	_ = first.c.Close()
	waitFor(t, "first owner gone", func() bool { return !f.owned(name) })

	second := newWireTap(t, f.sock)
	def := claudia.AgentDef{Provider: claudia.ProviderGrok}
	def.Name = name
	def.WorkDir = t.TempDir()
	def.SessionID = "sid-" + name
	def.TermLogPath = "-"
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: def})
	if err != nil {
		t.Fatal(err)
	}
	resp := second.call(&broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}})
	if resp.Type != broker.TypeGranted || !resp.Granted.Reclaimed {
		t.Fatalf("reclaim answered %s: %+v %+v", resp.Type, resp.Granted, resp.Error)
	}
	if d := f.d.reg.Def(name); d == nil || d.ProviderPick != claudia.PickRemaining {
		t.Fatalf("reclaim dropped the delegation: %+v", d)
	}
	second.submit("s1", name, "carry on", "")
	s := seats()[0]
	refuse(t, f, name, s)
	waitFor(t, "moved after reclaim", func() bool { return len(s.switched()) == 1 })
}

// A move that fails after the host has already submitted again must not
// forward the old refusal: it would end the newer turn as an error.
func TestT252FailedMoveAfterNewerSubmitDropsTheOldRefusal(t *testing.T) {
	f, seats := failoverFixture(t)
	decided := make(chan bool, 1)
	f.d.mu.Lock()
	f.d.failoverDecision = func(_ string, forwarded bool) { decided <- forwarded }
	f.d.mu.Unlock()
	w := newWireTap(t, f.sock)
	const name = "stale-refusal"
	w.grant(name, claudia.AgentDef{}, claudia.PickRemaining)
	w.submit("s1", name, "review the OCR fix", "")
	s := seats()[0]
	hold := make(chan struct{})
	s.mu.Lock()
	s.hold = hold
	s.mu.Unlock()
	refuse(t, f, name, s)
	waitFor(t, "move announced", func() bool { return len(progress(w.seen(), ProgressProviderFailover)) == 1 })

	w.submit("s2", name, "a newer task", "")
	close(hold)
	if forwarded := <-decided; forwarded {
		t.Fatal("the failed move forwarded a refusal older than the newer submit")
	}
	// Nothing was forwarded, and nothing earlier was either.
	if n := refusalsSeen(w.seen()); n != 0 {
		t.Fatalf("a stale refusal reached the newer turn: %+v", w.seen())
	}
	if got := s.sent(); len(got) != 2 || got[1] != "a newer task" {
		t.Fatalf("sends = %q", got)
	}
}

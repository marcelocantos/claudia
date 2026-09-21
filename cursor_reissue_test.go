// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// The 🎯T92 differential is deterministic, not statistical. cl-t33 showed
// saturation cannot decide this family — the pre-fix code passed 5/5 at
// load 44 — so the stimulus here is a peer the test speaks for, line by
// line, over in-process pipes. Nothing races the host's scheduler: the
// peer answers when this test writes its answer and not before, and every
// assertion is about an event that did or did not arrive.
//
// The one clock left is the product's own silence bound, which is what
// provokes the re-issue. That direction is safe: a bound too long only
// makes the test slower, and the peer is unconditionally silent on the
// delivery the bound is watching, so the verdict cannot depend on how
// fast the host was.
type acpTestPeer struct {
	t     *testing.T
	lines chan acpRPCMessage
	out   *io.PipeWriter
}

// pipedCursorClient returns a cursorACPClient whose transport is this
// test, with the same readLoop and wake path the real constructor
// installs.
//
// The peer drains the client's writes continuously, in its own
// goroutine. It must: io.Pipe hands off synchronously, so a peer that
// only reads when the test asks it to will wedge the client mid-write —
// the client's session/cancel blocks, the re-issue never goes out, and
// the harness reports a hang that is its own.
func pipedCursorClient(t *testing.T, onEvent func(Event)) (*cursorACPClient, *acpTestPeer) {
	t.Helper()
	peerReads, clientWrites := io.Pipe()
	clientReads, peerWrites := io.Pipe()
	c := &cursorACPClient{
		stdin:     clientWrites,
		stdout:    clientReads,
		pending:   make(map[int64]chan acpRPCMessage),
		peerWoke:  make(chan struct{}, 1),
		sessionID: pipedCursorSession,
		onEvent:   onEvent,
	}
	p := &acpTestPeer{t: t, lines: make(chan acpRPCMessage, 64), out: peerWrites}
	go c.readLoop()
	go func() {
		defer close(p.lines)
		sc := bufio.NewScanner(peerReads)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var msg acpRPCMessage
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			p.lines <- msg
		}
	}()
	t.Cleanup(func() {
		_ = clientWrites.Close()
		_ = peerWrites.Close()
		_ = peerReads.Close()
		_ = clientReads.Close()
	})
	return c, p
}

const pipedCursorSession = "sess-piped-cursor"

// nextPrompt blocks until the client writes a session/prompt and returns
// its id. Everything else the client writes is consumed on the way past:
// this is a peer, not an assertion about framing.
//
// The deadline is a backstop on a wedged build, never the verdict on a
// healthy one — the client writes the re-delivery as soon as its own
// bound expires, which this test has already caused.
func (p *acpTestPeer) nextPrompt() int64 {
	p.t.Helper()
	deadline := wallclockguard.UntilTestTimeout(p.t).Done()
	for {
		select {
		case msg, ok := <-p.lines:
			if !ok {
				p.t.Fatal("client transport closed before it wrote the session/prompt this test was waiting for")
			}
			if msg.Method == "session/prompt" && msg.ID != nil {
				return *msg.ID
			}
		case <-deadline:
			p.t.Fatal("client wrote no session/prompt")
		}
	}
}

// nextMethod blocks until the client writes the named method. Same
// backstop, same reason.
func (p *acpTestPeer) nextMethod(method string) acpRPCMessage {
	p.t.Helper()
	deadline := wallclockguard.UntilTestTimeout(p.t).Done()
	for {
		select {
		case msg, ok := <-p.lines:
			if !ok {
				p.t.Fatalf("client transport closed before it wrote %s", method)
			}
			if msg.Method == method {
				return msg
			}
		case <-deadline:
			p.t.Fatalf("client wrote no %s", method)
		}
	}
}

func (p *acpTestPeer) send(obj any) {
	p.t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.out.Write(append(b, '\n')); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

func (p *acpTestPeer) chunk(text string) {
	p.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": pipedCursorSession,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		},
	})
}

func (p *acpTestPeer) result(id int64, stopReason string) {
	p.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]any{"stopReason": stopReason},
	})
}

// turnSink collects the assistant events of one turn and reports when a
// terminal one has arrived. It is the oracle: "the caller has something
// to stop waiting for" is exactly an assistant event with a terminal stop
// reason, and "the reply was not lost" is exactly the text on the events
// that preceded it.
type turnSink struct {
	events   chan Event
	text     strings.Builder
	terminal bool
}

func newTurnSink() *turnSink { return &turnSink{events: make(chan Event, 64)} }

func (s *turnSink) onEvent(ev Event) {
	if ev.Type == "assistant" {
		s.events <- ev
	}
}

// awaitTerminal drains events until one is terminal. The deadline is a
// generous backstop on a broken build, never the verdict on a healthy
// one: a correct client publishes the terminal event as soon as the peer
// answers, which this test has already caused.
func (s *turnSink) awaitTerminal(t *testing.T) (string, bool) {
	t.Helper()
	deadline := wallclockguard.UntilTestTimeout(t).Done()
	for {
		select {
		case ev := <-s.events:
			appendTurnText(&s.text, ev)
			if ev.IsTerminalStop() {
				s.terminal = true
				return s.text.String(), true
			}
		case <-deadline:
			return s.text.String(), false
		}
	}
}

// 🎯T92, the reply the harness threw away. The peer is merely slow: it
// says nothing before the bound expires, the client gives up on that
// delivery and re-establishes the turn, and THEN the peer answers — the
// delivery the client abandoned.
//
// Before the fix the abandoned id was dropped from the prompt stack
// outright, so its result settled as acpPromptNotOurs: no terminal event
// of any kind. The caller was left waiting on a turn that had already
// been answered. Both faces of the observed failure follow from that one
// silence — `reply "", want pong` when something else ends the wait, and
// an indefinite park in WaitForResponse when nothing does.
func TestCursorReissueRedeemsLateReplyToAbandonedDelivery(t *testing.T) {
	shortenCursorSilenceBound(t, 200*time.Millisecond)
	sink := newTurnSink()
	c, peer := pipedCursorClient(t, sink.onEvent)

	done := make(chan error, 1)
	go func() { done <- c.Prompt("Reply with exactly: pong") }()

	first := peer.nextPrompt()
	// Say nothing. The bound expires, the client cancels and re-delivers.
	second := peer.nextPrompt()
	if second == first {
		t.Fatalf("re-issue reused prompt id %d", first)
	}
	// Now answer the FIRST delivery: the peer was slow, not deaf.
	peer.chunk("pong")
	peer.result(first, "end_turn")

	if err := <-done; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	text, ok := sink.awaitTerminal(t)
	if !ok {
		t.Fatalf("no terminal event for the surviving turn: the caller has nothing to stop waiting for "+
			"(text so far %q). This is the indefinite park in WaitForResponse.", text)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("reply %q, want pong: the peer's answer was discarded by the re-establish path", text)
	}
}

// The other half, and the reason a redeemed delivery cannot be taken on
// faith: the client's own session/cancel is answered too. A cancellation
// acknowledgement is not the turn's answer, and treating it as one would
// end the turn with an empty reply — `reply "", want pong` arriving by a
// second road. The surviving delivery still owes a terminal event, and
// gets to deliver it.
func TestCursorReissueDoesNotEndTurnOnCancellationAck(t *testing.T) {
	shortenCursorSilenceBound(t, 200*time.Millisecond)
	sink := newTurnSink()
	c, peer := pipedCursorClient(t, sink.onEvent)

	done := make(chan error, 1)
	go func() { done <- c.Prompt("Reply with exactly: pong") }()

	first := peer.nextPrompt()
	second := peer.nextPrompt()
	// The peer honours the cancel: the abandoned delivery ends with no
	// answer in it at all.
	peer.result(first, "cancelled")
	// ...and answers the delivery that survived.
	peer.chunk("pong")
	peer.result(second, "end_turn")

	if err := <-done; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	text, ok := sink.awaitTerminal(t)
	if !ok {
		t.Fatalf("no terminal event for the surviving turn (text so far %q)", text)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("reply %q, want pong: a cancellation acknowledgement was mistaken for the turn's answer", text)
	}
}

// A reply that lands in the window between the bound expiring and the
// re-delivery going out is the acceptance's own wording, and the peer
// gets a signal for it rather than a stopwatch: the client writes
// session/cancel on its way to re-delivering, so answering the instant
// that cancel appears puts the reply inside the window by construction.
//
// This is the guard on the ordering the fix depends on. The decision to
// abandon a delivery is taken before anything else goes on the wire, so
// by the time the peer can see the cancel, the abandoned id is already
// redeemable. Cancelling first — the old order — made the outcome a race
// between the reply and the stack swap.
func TestCursorReissueKeepsAReplyRacingTheRedelivery(t *testing.T) {
	shortenCursorSilenceBound(t, 200*time.Millisecond)
	sink := newTurnSink()
	c, peer := pipedCursorClient(t, sink.onEvent)

	done := make(chan error, 1)
	go func() { done <- c.Prompt("Reply with exactly: pong") }()

	first := peer.nextPrompt()
	peer.nextMethod("session/cancel")
	peer.chunk("pong")
	peer.result(first, "end_turn")

	if err := <-done; err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	text, ok := sink.awaitTerminal(t)
	if !ok {
		t.Fatalf("no terminal event (text so far %q)", text)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("reply %q, want pong", text)
	}
}

// The seat stays usable when the peer really is silent. 🎯T83's own
// guarantee must survive the redeem path: two silent deliveries is still
// a typed error and an idle seat, not a turn left half open.
func TestCursorReissueStillFailsTypedWhenBothDeliveriesAreSilent(t *testing.T) {
	shortenCursorSilenceBound(t, 100*time.Millisecond)
	sink := newTurnSink()
	c, peer := pipedCursorClient(t, sink.onEvent)

	done := make(chan error, 1)
	go func() { done <- c.Prompt("the opening brief") }()

	peer.nextPrompt()
	peer.nextPrompt()

	err := <-done
	if !errors.Is(err, ErrCursorPromptStuck) {
		t.Fatalf("Prompt err = %v, want errors.Is ErrCursorPromptStuck", err)
	}
	if c.promptInFlight() {
		t.Fatal("a stuck mint left a turn in flight; every later Send would read ErrTurnInFlight")
	}
}

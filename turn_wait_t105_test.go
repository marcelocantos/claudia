// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// 🎯T105. An unrelayable broker frame is one lost event, not a dead
// connection (🎯T73), and 🎯T96's silence bound is what keeps that hole from
// parking a wait forever. But when the lost frame was the turn's terminal
// event, the wait used to end on ErrTurnAbandoned alone — "nothing at all
// arrived" — which was false: the agent answered and the wire dropped it.
//
// Every test here drives a ManualClock, as turn_wait_test.go does. The drop
// itself is real: a brokerClient over a pipe reads a frame too large for the
// wire and skips it.

// droppingSeat is a wait fixture whose drop count comes from a real broker
// client, as a broker-held seat's does.
func droppingSeat(t *testing.T, bound time.Duration) (*Agent, *ManualClock, *brokerClient, *errPeer) {
	t.Helper()
	clk := NewManualClock(time.Now())
	a := waitFixture(clk, bound, false)
	client, pipe := clientOverPipe(t)
	a.ops.droppedFrames = func(*Agent) (int, error) { return client.DroppedFrames() }
	return a, clk, client, &errPeer{conn: pipe, errc: peerErrs()}
}

type errPeer struct {
	conn interface{ Write([]byte) (int, error) }
	errc chan error
}

// dropTerminalEvent sends the seat's terminal event as a frame the wire
// cannot carry, and returns once the client has skipped it. The frame is a
// real agent_event line whose text is padded past the line limit — a peer
// that does not bound what it relays.
func (p *errPeer) dropTerminalEvent(t *testing.T, client *brokerClient) {
	t.Helper()
	before, _ := client.DroppedFrames()
	raw, err := EncodeEventWire(Event{Type: "assistant", SessionID: "t96-session", TurnID: "turn-9", Text: "PAD", StopReason: "end_turn"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := (&broker.Response{Type: broker.TypeAgentEvent,
		AgentEvent: &broker.AgentEventMessage{Name: "seat", Event: raw}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	line = bytes.Replace(line, []byte("PAD"), bytes.Repeat([]byte("x"), 3*broker.MaxLineLen), 1)
	go func() {
		if _, err := p.conn.Write(append(line, '\n')); err != nil {
			notePeerErr(p.errc, err)
		}
	}()
	for {
		if n, _ := client.DroppedFrames(); n > before {
			return
		}
		runtime.Gosched()
	}
}

// The acceptance: a terminal event dropped as an oversized frame, then the
// silence bound. The error still matches ErrTurnAbandoned, and it names
// the drop instead of claiming nothing arrived.
func TestAbandonedTurnAfterADroppedTerminalNamesTheDrop(t *testing.T) {
	const bound = 90 * time.Second
	a, clk, client, peer := droppingSeat(t, bound)

	res := startWait(t, a)
	a.publishEvent(Event{Type: "assistant", SessionID: "t96-session", TurnID: "turn-9", Text: "working"})
	peer.dropTerminalEvent(t, client)

	r := advanceUntilAnswered(clk, bound, res)
	if !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned — the wait still ends on the bound", r.err)
	}
	if !errors.Is(r.err, ErrFramesDropped) {
		t.Fatalf("err = %v, want errors.Is ErrFramesDropped: a turn whose end was lost on the wire reads as an agent that went silent", r.err)
	}
	for _, want := range []string{"1 broker frame(s)", "dropped as too large to relay", "not evidence the agent stopped"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error %q does not say %q", r.err, want)
		}
	}
	noPeerError(t, peer.errc)
}

// The other half: a genuinely silent agent on a broker connection that lost
// nothing is reported as silent, with no drop claimed.
func TestAbandonedTurnWithoutADropClaimsNone(t *testing.T) {
	const bound = 90 * time.Second
	a, clk, _, _ := droppingSeat(t, bound)

	res := startWait(t, a)
	a.publishEvent(Event{Type: "assistant", SessionID: "t96-session", TurnID: "turn-9", Text: "working"})

	r := advanceUntilAnswered(clk, bound, res)
	if !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned", r.err)
	}
	if errors.Is(r.err, ErrFramesDropped) || strings.Contains(r.err.Error(), "dropped") {
		t.Fatalf("err = %v claims a drop on a connection that lost nothing", r.err)
	}
}

// A frame lost before this wait began belongs to some earlier turn. Only the
// drops during the wait are this turn's, so an old hole is not blamed for a
// new silence.
func TestADropBeforeTheWaitIsNotThisTurns(t *testing.T) {
	const bound = 90 * time.Second
	a, clk, client, peer := droppingSeat(t, bound)
	peer.dropTerminalEvent(t, client)

	res := startWait(t, a)
	a.publishEvent(Event{Type: "assistant", SessionID: "t96-session", TurnID: "turn-10", Text: "working"})

	r := advanceUntilAnswered(clk, bound, res)
	if !errors.Is(r.err, ErrTurnAbandoned) {
		t.Fatalf("err = %v, want errors.Is ErrTurnAbandoned", r.err)
	}
	if errors.Is(r.err, ErrFramesDropped) {
		t.Fatalf("err = %v blames this turn's silence on a frame lost before it began", r.err)
	}
	noPeerError(t, peer.errc)
}

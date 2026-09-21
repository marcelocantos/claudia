// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// The consumer-side survival oracle for 🎯T73.
//
// This is the jevons 🎯T661 incident in miniature. One connection carries a
// consumer's requests and every push for its seats. A screenshot arrived as a
// push the wire could not frame, and the read loop treated the refusal as the
// connection dying: a 1.2 KB send that had nothing to do with the screenshot
// failed, along with everything else in flight. The contract now is that the
// frame is lost and nothing else is.

// clientOverPipe is a brokerClient wired to a raw peer. The peer is a plain
// net.Conn on purpose: writing bytes the framing itself would refuse is how a
// test reproduces a peer that does not share our limits.
// A peer goroutine here reports through this channel and never through t, for
// the reason spelled out in internal/broker/transport_test.go: a test that
// gives up on the connection leaves the peer blocked mid-write, and the
// t.Errorf it reaches after cleanup closes the pipe panics the package and
// buries the assertion that failed. These tests fail by abandoning a
// connection more often than most, because that is the defect they pin.
func peerErrs() chan error { return make(chan error, 1) }

// notePeerErr records the peer's first write error without touching t.
func notePeerErr(errc chan<- error, err error) {
	select {
	case errc <- err:
	default:
	}
}

// noPeerError reports a peer failure while the test is still running.
func noPeerError(t *testing.T, errc <-chan error) {
	t.Helper()
	select {
	case err := <-errc:
		t.Errorf("peer: %v", err)
	default:
	}
}

func clientOverPipe(t *testing.T) (*brokerClient, net.Conn) {
	t.Helper()
	ours, theirs := net.Pipe()
	b := &brokerClient{
		conn:    broker.NewConn(ours),
		pending: map[string]chan *broker.Response{},
		done:    make(chan struct{}),
	}
	go b.readLoop()
	t.Cleanup(func() { b.Close(); _ = theirs.Close() })
	return b, theirs
}

// TestOversizedPushDoesNotKillThePendingRequest is the acceptance oracle:
// an unrelayable push is dropped, the pending request on the same connection
// still completes, and the client can tell that it lost a frame.
func TestOversizedPushDoesNotKillThePendingRequest(t *testing.T) {
	b, peer := clientOverPipe(t)

	errc := peerErrs()
	go func() {
		// Read the request the caller is waiting on.
		req, err := broker.NewConn(peer).ReadRequest()
		if err != nil {
			notePeerErr(errc, err)
			return
		}
		// A screenshot push lands first, too large for one frame.
		if _, err := peer.Write(append(bytes.Repeat([]byte("p"), 3*broker.MaxLineLen), '\n')); err != nil {
			notePeerErr(errc, err)
			return
		}
		// Then the answer the caller is waiting on, on the same connection.
		line, err := (&broker.Response{ID: req.ID, Type: broker.TypeStatusResult,
			Status: &broker.StatusResponse{}}).Encode()
		if err != nil {
			notePeerErr(errc, err)
			return
		}
		if _, err := peer.Write(append(line, '\n')); err != nil {
			notePeerErr(errc, err)
		}
	}()

	resp, err := b.callTimeout(&broker.Request{Type: broker.TypeStatus}, 5*time.Second)
	if err != nil {
		t.Fatalf("the pending request died with the oversized push: %v", err)
	}
	if resp.Type != broker.TypeStatusResult {
		t.Fatalf("got %s, want %s", resp.Type, broker.TypeStatusResult)
	}

	dropped, last := b.DroppedFrames()
	if dropped != 1 {
		t.Errorf("dropped frame count is %d, want 1", dropped)
	}
	if !errors.Is(last, broker.ErrFrameTooLarge) {
		t.Errorf("the recorded drop is not an oversized frame: %v", last)
	}
	noPeerError(t, errc)
}

// TestOversizedPushStillDeliversLaterPushes pins that the push stream itself
// survives, not merely the request channel: the events after the hole arrive.
func TestOversizedPushStillDeliversLaterPushes(t *testing.T) {
	b, peer := clientOverPipe(t)

	got := make(chan *broker.Response, 4)
	b.setPush(func(r *broker.Response) { got <- r })

	raw, err := EncodeEventWire(Event{Type: "assistant", Text: "after the hole"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := (&broker.Response{Type: broker.TypeAgentEvent,
		AgentEvent: &broker.AgentEventMessage{Name: "cl-worker-1", Event: raw}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	errc := peerErrs()
	go func() {
		if _, err := peer.Write(append(bytes.Repeat([]byte("p"), 3*broker.MaxLineLen), '\n')); err != nil {
			notePeerErr(errc, err)
			return
		}
		if _, err := peer.Write(append(line, '\n')); err != nil {
			notePeerErr(errc, err)
		}
	}()

	select {
	case r := <-got:
		if r.AgentEvent == nil {
			t.Fatalf("want an agent_event, got %+v", r)
		}
		ev, err := DecodeEventWire(r.AgentEvent.Event)
		if err != nil || ev.Text != "after the hole" {
			t.Fatalf("event after the hole: %+v, %v", ev, err)
		}
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatal("the push stream stopped at the oversized frame")
	}
	noPeerError(t, errc)
}

// TestBrokerClientStillFailsOnARealClose keeps the skip narrow. An oversized
// frame is survivable; a peer that hangs up is not, and a read loop that
// swallowed both would hang every caller forever.
func TestBrokerClientStillFailsOnARealClose(t *testing.T) {
	b, peer := clientOverPipe(t)
	errc := peerErrs()
	go func() {
		if _, err := broker.NewConn(peer).ReadRequest(); err != nil {
			notePeerErr(errc, err)
			return
		}
		_ = peer.Close()
	}()
	if _, err := b.callTimeout(&broker.Request{Type: broker.TypeStatus}, 5*time.Second); err == nil {
		t.Fatal("a closed connection did not fail the pending request")
	}
	noPeerError(t, errc)
}

// TestRelayedScreenshotSurvivesAWholeRoundTrip is the two halves together:
// the daemon's codec bounds a screenshot line, the transport carries it, and
// the consumer decodes an event that says it was bounded. Either half alone
// leaves the incident in place — bounding without a surviving reader still
// dies on the next unbounded producer, and a surviving reader without
// bounding silently loses every screenshot.
func TestRelayedScreenshotSurvivesAWholeRoundTrip(t *testing.T) {
	b, peer := clientOverPipe(t)
	got := make(chan *broker.Response, 1)
	b.setPush(func(r *broker.Response) { got <- r })

	raw, err := EncodeEventWire(Event{Type: "user", Raw: screenshotToolResult(t, 2, 600<<10)})
	if err != nil {
		t.Fatal(err)
	}
	server := broker.NewConn(peer)
	errc := peerErrs()
	go func() {
		if err := server.WriteResponse(&broker.Response{Type: broker.TypeAgentEvent,
			AgentEvent: &broker.AgentEventMessage{Name: "cl-worker-1", Event: raw}}); err != nil {
			notePeerErr(errc, err)
		}
	}()

	select {
	case r := <-got:
		ev, err := DecodeEventWire(r.AgentEvent.Event)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !ev.Truncated {
			t.Error("the relayed screenshot does not report itself bounded")
		}
		if !json.Valid(ev.Raw) {
			t.Fatal("the relayed payload is not valid JSON")
		}
		if !strings.Contains(string(ev.Raw), "[claudia: elided ") {
			t.Error("the relayed payload does not name what it lost")
		}
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatal("the bounded screenshot never arrived")
	}

	if dropped, _ := b.DroppedFrames(); dropped != 0 {
		t.Errorf("a bounded screenshot was still dropped as oversized (%d)", dropped)
	}
	noPeerError(t, errc)
}

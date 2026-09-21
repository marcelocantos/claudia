// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestT125FullPushQueueDoesNotStallRequests: a seat whose handle is not
// draining (nobody has subscribed yet, or its consumer is slow) must not
// stop the connection's read loop, because that loop also reads the answers
// to the seat's own Send/Interrupt. The old queue was a 4096-slot channel
// that blocked the loop when full; 5000 pushes ahead of a reply meant the
// reply was never read. Restoring a bounded blocking queue turns this red.
func TestT125FullPushQueueDoesNotStallRequests(t *testing.T) {
	b, peer := clientOverPipe(t)
	backend := &brokerAgentBackend{client: b}
	backend.queue.init()
	b.setPush(backend.onPush)

	raw, err := EncodeEventWire(Event{Type: "progress", ProgressType: "tool_use"})
	if err != nil {
		t.Fatal(err)
	}
	const pushes = 5000 // more than the old 4096-slot queue
	server := broker.NewConn(peer)
	errc := peerErrs()
	go func() {
		req, err := server.ReadRequest()
		if err != nil {
			notePeerErr(errc, err)
			return
		}
		for i := 0; i < pushes; i++ {
			if err := server.WriteResponse(&broker.Response{Type: broker.TypeAgentEvent,
				AgentEvent: &broker.AgentEventMessage{Name: "s", Event: raw}}); err != nil {
				notePeerErr(errc, err)
				return
			}
		}
		if err := server.WriteResponse(&broker.Response{ID: req.ID, Type: broker.TypeSent, Sent: &broker.SentResponse{Name: "s"}}); err != nil {
			notePeerErr(errc, err)
		}
	}()

	if _, err := b.callTimeout(&broker.Request{Type: broker.TypeSend, Send: &broker.SendRequest{Name: "s", Text: "x"}}, 10*time.Second); err != nil {
		t.Fatalf("Send reply stuck behind %d undelivered pushes: %v", pushes, err)
	}
	got := 0
	for {
		if _, ok := backend.queue.pop(); !ok {
			break
		}
		got++
	}
	if got != pushes {
		t.Fatalf("queued %d pushes, want %d (the queue must not drop)", got, pushes)
	}
	noPeerError(t, errc)
}

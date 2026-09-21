// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// heldPump is a pumpGate that blocks every pump write until released, which
// models a consumer that has stopped reading without depending on socket
// buffer sizes.
func heldPump(t *testing.T) (gate func(), release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	return func() { <-ch }, release
}

// TestT125DetachIsAnnounced: when a seat's event queue overflows the daemon
// detaches the consumer and says so, so the consumer's next Send is not a
// bare not_owner. Removing notifyDetached turns this red (🎯T125).
func TestT125DetachIsAnnounced(t *testing.T) {
	f := newFixture(t)
	gate, release := heldPump(t)
	opts := f.options(nil)
	opts.DisableResume, opts.pumpSize, opts.pumpGate = true, 2, gate
	f.bootWith(t, opts)
	// Registered after the daemon's Close so it runs first: a failed
	// assertion must not leave the pump holding Close.
	t.Cleanup(release)
	c := rawSeat(t, f.sock, "slow")
	proc := f.d.reg.Get("slow")
	for i := 0; i < 20; i++ {
		proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("t%d", i)})
	}
	waitFor(t, "detached", func() bool { return !f.owned("slow") })
	release()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatalf("consumer never told it was detached: %v", err)
		}
		if resp.Type == broker.TypeAgentDetached {
			if resp.AgentDetached.Name != "slow" || resp.AgentDetached.Reason == "" {
				t.Fatalf("detach message = %+v", resp.AgentDetached)
			}
			return
		}
	}
}

// TestT125ReplayBurstDoesNotDetach: a launch or adopt of a large session
// streams its whole transcript through the queue while the write side is
// slow. 4100 is the transcript line count of the overseer session that
// overflowed the old 1024-slot queue at load ~377; every line yields at
// least one event, so 4100 events is the floor of the measured burst.
func TestT125ReplayBurstDoesNotDetach(t *testing.T) {
	f := newFixture(t)
	gate, release := heldPump(t)
	opts := f.options(nil)
	opts.DisableResume, opts.pumpGate = true, gate
	f.bootWith(t, opts)
	// Registered after the daemon's Close so it runs first: a failed
	// assertion must not leave the pump holding Close.
	t.Cleanup(release)
	c := rawSeat(t, f.sock, "burst")
	proc := f.d.reg.Get("burst")
	const burst = 4100
	for i := 0; i < burst; i++ {
		proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("t%d", i)})
	}
	if !f.owned("burst") {
		t.Fatalf("seat detached during a %d-event burst", burst)
	}
	release()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	for got := 0; got < burst; {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatalf("after %d of %d events: %v", got, burst, err)
		}
		switch resp.Type {
		case broker.TypeAgentEvent:
			got++
		case broker.TypeAgentDetached:
			t.Fatalf("detached after %d of %d events", got, burst)
		}
	}
}

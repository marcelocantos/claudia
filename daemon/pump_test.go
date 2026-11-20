// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestReclaimAfterDisconnectKeepsPumpEventsInOrder(t *testing.T) {
	const name = "pump-reclaim"
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t)
	gate, release := heldPump(t)
	entered := make(chan struct{})
	var once sync.Once
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	opts.pumpGate = func() {
		once.Do(func() { close(entered) })
		gate()
	}
	f.bootWith(t, opts)
	t.Cleanup(release)
	old := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	publishReclaimEvent(proc, "e0")
	// entered closes when the pump takes e0. go test -timeout is the clock.
	<-entered
	publishReclaimEvent(proc, "e1")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "owner disconnect", func() bool { return !f.owned(name) })
	publishReclaimEvent(proc, "e2")
	// The old writer can still be in flight when the next owner arrives.
	assertGrantReplay(t, dialRaw(t, f.sock), reclaimRequest(t, f, name), []string{"e0", "e1", "e2"})
}

// TestReclaimPlaysHistoryBeforeALiveEvent pins H11. The seat publishes
// again as soon as the reclaim takes ownership, which is before the ring
// has been written. That event must follow the history and the grant
// reply. The pump starts only after both.
func TestReclaimPlaysHistoryBeforeALiveEvent(t *testing.T) {
	const name = "order"
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	old := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "owner disconnect", func() bool { return !f.owned(name) })

	const history = 64
	want := make([]string, history)
	for i := range want {
		want[i] = fmt.Sprintf("h%04d", i)
		publishReclaimEvent(proc, want[i])
	}

	c := dialRaw(t, f.sock)
	req := reclaimRequest(t, f, name)
	if err := c.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reclaim to take ownership", func() bool { return f.owned(name) })
	publishReclaimEvent(proc, "LIVE")

	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if resp.Type == broker.TypeGranted {
			break
		}
		got = append(got, replayTitle(t, resp))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events before the grant reply = %v, want history %v and no LIVE", got, want)
	}
	if got := replayTitle(t, mustRead(t, c)); got != "LIVE" {
		t.Fatalf("first event after the grant reply = %q, want LIVE", got)
	}
}

// TestSlowConsumerReclaimKeepsEventOrder is H12. The pump is held before
// its first write, the queue overflows, and the same connection re-claims.
// The event blocked in the gate was not written, so the replay is the
// whole sequence, once, in order.
func TestSlowConsumerReclaimKeepsEventOrder(t *testing.T) {
	const name = "slow"
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t)
	gate, release := heldPump(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost, opts.pumpSize, opts.pumpGate = true, true, 2, gate
	f.bootWith(t, opts)
	t.Cleanup(release)

	c := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	f.d.mu.Lock()
	pump := f.d.grants[name].pump
	f.d.mu.Unlock()
	publishReclaimEvent(proc, "e0")
	waitFor(t, "pump to dequeue e0", func() bool { return len(pump.queue) == 0 })
	publishReclaimEvent(proc, "e1")
	publishReclaimEvent(proc, "e2")
	publishReclaimEvent(proc, "e3")
	waitFor(t, "slow consumer detached", func() bool { return !f.owned(name) })
	publishReclaimEvent(proc, "e4")

	req := reclaimRequest(t, f, name)
	if err := c.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	release()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if resp.ID == req.ID && resp.Type == broker.TypeGranted {
			break
		}
		if resp.Type != broker.TypeAgentEvent {
			continue
		}
		got = append(got, replayTitle(t, resp))
	}
	want := []string{"e0", "e1", "e2", "e3", "e4"}
	if !slices.Equal(got, want) {
		t.Fatalf("consumer saw %v, want %v", got, want)
	}
}

// TestLiveDetachDuringWriteDeliversEachEventOnce is the live-connection
// half of 🎯T1404. Detach falls in the window after the pump has committed
// to a write and before that write returns. The connection already has the
// event, so the reclaim must not replay it. An unread kernel buffer after
// the process has gone is not this case.
func TestLiveDetachDuringWriteDeliversEachEventOnce(t *testing.T) {
	const name = "once"
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t)
	entered := make(chan struct{})
	releaseWrite := make(chan struct{})
	var once sync.Once
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost, opts.pumpSize = true, true, 2
	opts.pumpDuringWrite = func() {
		once.Do(func() { close(entered) })
		<-releaseWrite
	}
	f.bootWith(t, opts)

	c := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	publishReclaimEvent(proc, "e0")
	<-entered
	publishReclaimEvent(proc, "e1")
	publishReclaimEvent(proc, "e2")
	publishReclaimEvent(proc, "e3")
	waitFor(t, "detached during the write", func() bool { return !f.owned(name) })
	close(releaseWrite)
	waitFor(t, "writer finished the in-flight event", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants[name]
		return g != nil && g.draining == nil && g.pump == nil
	})

	buffered := readUntilIdle(t, c)
	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	req := reclaimRequest(t, f, name)
	if err := c.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var replayed []string
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if resp.ID == req.ID && resp.Type == broker.TypeGranted {
			break
		}
		if resp.Type != broker.TypeAgentEvent {
			continue
		}
		replayed = append(replayed, replayTitle(t, resp))
	}
	got := append(buffered, replayed...)
	want := []string{"e0", "e1", "e2", "e3"}
	if !slices.Equal(got, want) {
		t.Fatalf("consumer saw %v, want %v (buffered %v, replayed %v)", got, want, buffered, replayed)
	}
}

// readUntilIdle reads agent-event titles already buffered on c, and stops
// when the connection stays quiet. go test -timeout is the outer clock.
func readUntilIdle(t *testing.T, c *broker.Conn) []string {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			if isTimeout(err) {
				return got
			}
			t.Fatal(err)
		}
		if resp.Type != broker.TypeAgentEvent {
			continue
		}
		got = append(got, replayTitle(t, resp))
	}
}

func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

func mustRead(t *testing.T, c *broker.Conn) *broker.Response {
	t.Helper()
	resp, err := c.ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestDetachReportsLagWhenPumpQueueExceedsReclaimCapacity(t *testing.T) {
	d := &Daemon{}
	pump := d.newGrantPump()
	pump.writing = []byte("oldest")
	for range brokerUnownedRingCap - 1 {
		pump.queue <- []byte("retained")
	}
	pump.queue <- []byte("newest")
	g := &brokerGrant{pump: pump}
	d.mu.Lock()
	d.detachLocked(g)
	d.mu.Unlock()
	if !g.lag || len(g.ring) != brokerUnownedRingCap {
		t.Fatalf("ring count=%d lagged=%v, want %d and true", len(g.ring), g.lag, brokerUnownedRingCap)
	}
	if first, last := string(g.ring[0]), string(g.ring[len(g.ring)-1]); first != "retained" || last != "newest" {
		t.Fatalf("ring starts with %q and ends with %q, want retained through newest", first, last)
	}
}

func TestReclaimAfterReplayDisconnectKeepsHistory(t *testing.T) {
	for _, cut := range []struct {
		name string
		read int
	}{
		{"during-replay", 1},
		{"before-granted", 3},
	} {
		t.Run(cut.name, func(t *testing.T) {
			const name = "replay-reclaim"
			t.Setenv("HOME", t.TempDir())
			f := newFixture(t)
			opts := f.options(nil)
			opts.DisableResume, opts.DisableMCPHost = true, true
			f.bootWith(t, opts)
			old := rawSeat(t, f.sock, name)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "owner disconnect", func() bool { return !f.owned(name) })
			proc := f.d.reg.Get(name)
			history := []string{"e0", "e1", "e2"}
			for _, title := range history {
				publishReclaimEvent(proc, title)
			}
			req := reclaimRequest(t, f, name)

			// A synchronous pipe lets the reclaimer close at an exact frame
			// boundary, even when the entire replay would fit in a socket buffer.
			server, client := net.Pipe()
			crashed := broker.NewConn(client)
			t.Cleanup(func() { _ = crashed.Close() })
			if err := crashed.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			handled := make(chan struct{})
			go func() {
				defer close(handled)
				defer server.Close()
				conn := &broker.ClientConn{Conn: broker.NewConn(server)}
				defer f.d.ConnClosed(conn)
				f.d.handleGrant(conn, req)
			}()
			for _, want := range history[:cut.read] {
				resp, err := crashed.ReadResponse()
				if err != nil {
					t.Fatal(err)
				}
				if got := replayTitle(t, resp); got != want {
					t.Fatalf("first replay event = %q, want %q", got, want)
				}
			}
			publishReclaimEvent(proc, "e3")
			if err := crashed.Close(); err != nil {
				t.Fatal(err)
			}
			// handled closes when handleGrant returns. go test -timeout is the clock.
			<-handled
			replacement := dialRaw(t, f.sock)
			assertGrantReplay(t, replacement, req, append(history, "e3"))
			if err := replacement.Close(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "replacement disconnect", func() bool { return !f.owned(name) })
			assertGrantReplay(t, dialRaw(t, f.sock), req, nil)
		})
	}
}

func publishReclaimEvent(proc *claudia.Agent, title string) {
	proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: title})
}

func reclaimRequest(t *testing.T, f *fixture, name string) *broker.Request {
	t.Helper()
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: *f.d.reg.Def(name)})
	if err != nil {
		t.Fatal(err)
	}
	return &broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}}
}

func replayTitle(t *testing.T, resp *broker.Response) string {
	t.Helper()
	if resp.Type != broker.TypeAgentEvent {
		t.Fatalf("waiting for agent event, got %s: %+v", resp.Type, resp.Error)
	}
	ev, err := claudia.DecodeEventWire(resp.AgentEvent.Event)
	if err != nil {
		t.Fatal(err)
	}
	return ev.ToolTitle
}

func assertGrantReplay(t *testing.T, c *broker.Conn, req *broker.Request, want []string) {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if resp.Type == broker.TypeGranted {
			if !slices.Equal(got, want) || resp.Granted.Replayed != len(want) || resp.Granted.Lagged {
				t.Fatalf("replay = %v, count=%d lagged=%v, want %v with no lag",
					got, resp.Granted.Replayed, resp.Granted.Lagged, want)
			}
			return
		}
		got = append(got, replayTitle(t, resp))
	}
}

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
	t.Setenv("HOME", t.TempDir())
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
	want := make([]string, 20)
	for i := range want {
		want[i] = fmt.Sprintf("t%d", i)
		publishReclaimEvent(proc, want[i])
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
			break
		}
	}
	assertGrantReplay(t, c, reclaimRequest(t, f, "slow"), want)
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

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// C1/C2 on a real unix-socket connection: the consumer dies where the daemon
// can see it (before a pump write, or part way through a reclaim replay too
// large for the socket buffers). Every event the dead consumer did not read
// must be replayed to the next owner in order, or the reclaim reports Lagged.
// A consumer that dies after the daemon's writes landed in its socket buffer
// is 🎯T1404: the protocol has no acknowledgement to tell the daemon.

func killPublish(proc *claudia.Agent, i, pad int) {
	proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use",
		ToolTitle: fmt.Sprintf("e%04d%s", i, strings.Repeat(".", pad))})
}

func killIndex(t *testing.T, title string) int {
	var i int
	if _, err := fmt.Sscanf(title[:5], "e%04d", &i); err != nil {
		t.Fatalf("title %q: %v", title, err)
	}
	return i
}

// killReclaim reclaims name on a fresh real connection and returns the
// replayed indices and the Granted reply.
func killReclaim(t *testing.T, f *fixture, name string) ([]int, *broker.GrantResponse) {
	t.Helper()
	c := dialRaw(t, f.sock)
	if err := c.WriteRequest(reclaimRequest(t, f, name)); err != nil {
		t.Fatal(err)
	}
	var got []int
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if resp.Type == broker.TypeGranted {
			return got, resp.Granted
		}
		if resp.Type == broker.TypeAgentEvent {
			got = append(got, killIndex(t, replayTitle(t, resp)))
		}
	}
}

// assertUnreadReplayed: read are the indices the dead consumer actually read;
// every other index in [0,n) must be in replay, in order, or Lagged is set.
func assertUnreadReplayed(t *testing.T, n int, read, replay []int, g *broker.GrantResponse) {
	t.Helper()
	if g.Replayed != len(replay) {
		t.Fatalf("Replayed=%d but %d events were replayed", g.Replayed, len(replay))
	}
	seen := map[int]bool{}
	for _, i := range read {
		seen[i] = true
	}
	for k := 1; k < len(replay); k++ {
		if replay[k] <= replay[k-1] {
			t.Fatalf("replay out of order at %d: %v", k, replay)
		}
	}
	for _, i := range replay {
		seen[i] = true
	}
	var missing []int
	for i := 0; i < n; i++ {
		if !seen[i] {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 && !g.Lagged {
		t.Fatalf("%d of %d events never reached any consumer (first %d), replayed=%d lagged=false; dead consumer read %d",
			len(missing), n, missing[0], g.Replayed, len(read))
	}
}

func killBoot(t *testing.T, gate func()) *fixture {
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost, opts.pumpGate = true, true, gate
	f.bootWith(t, opts)
	return f
}

// Before send: the pump has taken e0 but not written it; e1..e9 queue.
func TestRealSocketKillBeforeSend(t *testing.T) {
	const name, n = "kill-before", 11
	gate, release := heldPump(t)
	f := killBoot(t, gate)
	t.Cleanup(release)
	c := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	for i := 0; i < 10; i++ {
		killPublish(proc, i, 0)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "detach", func() bool { return f.unowned(name) })
	release()
	killPublish(proc, 10, 0)
	replay, g := killReclaim(t, f, name)
	assertUnreadReplayed(t, n, nil, replay, g)
}

// C2, reclaimer dies before the daemon writes: the grant request is sent and
// the connection closed at once.
func TestRealSocketReclaimerDiesBeforeReplay(t *testing.T) {
	const name, n = "c2-before", 50
	f := killBoot(t, nil)
	_ = rawSeat(t, f.sock, name).Close()
	waitFor(t, "detach", func() bool { return f.unowned(name) })
	proc := f.d.reg.Get(name)
	for i := 0; i < n; i++ {
		killPublish(proc, i, 0)
	}
	crashed := dialRaw(t, f.sock)
	// The seat is already unowned, so "owner == nil" is true before this
	// grant is read. The generation moves when the handler takes the seat;
	// the close then gives it back. Sample before the write, which the
	// handler can finish before WriteRequest returns.
	gen := f.orphanGen(name)
	if err := crashed.WriteRequest(reclaimRequest(t, f, name)); err != nil {
		t.Fatal(err)
	}
	_ = crashed.Close()
	waitFor(t, "crashed grant handled", func() bool {
		return f.orphanGen(name) != gen && f.unowned(name)
	})
	replay, g := killReclaim(t, f, name)
	assertUnreadReplayed(t, n, nil, replay, g)
}

// C2, two crashes mid-replay: a replay too large for the socket buffers, the
// reclaimer reads five frames and dies; new events arrive; the second
// reclaimer reads five and dies; the third must still see everything.
func TestRealSocketReclaimerDiesMidReplayTwice(t *testing.T) {
	const name, n, pad = "c2-mid", 600, 2048
	f := killBoot(t, nil)
	_ = rawSeat(t, f.sock, name).Close()
	waitFor(t, "detach", func() bool { return f.unowned(name) })
	proc := f.d.reg.Get(name)
	next := 0
	for ; next < n; next++ {
		killPublish(proc, next, pad)
	}
	var read []int
	for crash := 0; crash < 2; crash++ {
		c := dialRaw(t, f.sock)
		if err := c.WriteRequest(reclaimRequest(t, f, name)); err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 5; k++ {
			resp, err := c.ReadResponse()
			if err != nil {
				t.Fatal(err)
			}
			read = append(read, killIndex(t, replayTitle(t, resp)))
		}
		_ = c.Close()
		waitFor(t, "crashed reclaim released", func() bool { return f.unowned(name) })
		killPublish(proc, next, pad)
		next++
	}
	replay, g := killReclaim(t, f, name)
	assertUnreadReplayed(t, next, read, replay, g)
}

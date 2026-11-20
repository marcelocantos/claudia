// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestAnotherConsumerTakesASeatFromAWedgedWriter: a consumer that stops
// reading is detached while the pump is inside a write to it, and that write
// does not return while the consumer stays wedged. A second consumer that
// claims the seat must be granted without waiting on the first one's
// socket, and it replays everything the wedged consumer may not have read,
// the in-flight event first. Waiting for that write is right only for the
// connection the write is going to (H12); for anyone else it is a grant
// that never answers.
func TestAnotherConsumerTakesASeatFromAWedgedWriter(t *testing.T) {
	const name = "wedged"
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t)
	entered := make(chan struct{})
	releaseWrite := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWrite) }) }
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost, opts.pumpSize = true, true, 2
	opts.pumpDuringWrite = func() {
		enterOnce.Do(func() { close(entered) })
		<-releaseWrite
	}
	f.bootWith(t, opts)
	// Registered after the daemon's Close so it runs first.
	t.Cleanup(release)

	rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	f.d.mu.Lock()
	wedged := f.d.grants[name].pump
	f.d.mu.Unlock()
	publishReclaimEvent(proc, "e0")
	<-entered
	publishReclaimEvent(proc, "e1")
	publishReclaimEvent(proc, "e2")
	publishReclaimEvent(proc, "e3")
	waitFor(t, "wedged consumer detached", func() bool { return !f.owned(name) })

	other, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	req := reclaimRequest(t, f, name)
	if err := other.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	if err := other.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		resp, err := other.ReadResponse()
		if err != nil {
			t.Fatalf("second consumer's grant never answered while the first consumer's write is held: %v (events so far %v)", err, got)
		}
		if resp.ID == req.ID && resp.Type == broker.TypeGranted {
			break
		}
		if resp.Type != broker.TypeAgentEvent {
			continue
		}
		got = append(got, replayTitle(t, resp))
	}
	if want := []string{"e0", "e1", "e2", "e3"}; !slices.Equal(got, want) {
		t.Fatalf("second consumer replayed %v, want %v", got, want)
	}

	// The wedged writer finishing later must not put its event back on a
	// ring the second consumer has already taken.
	release()
	<-wedged.done
	f.d.mu.Lock()
	ring := len(f.d.grants[name].ring)
	f.d.mu.Unlock()
	if ring != 0 {
		t.Fatalf("ring holds %d event(s) after the wedged writer finished; the second consumer already has them", ring)
	}
}

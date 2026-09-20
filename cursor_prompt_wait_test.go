// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 🎯T91. The 🎯T83 silence watch reads peerSeq, and peerSeq only moves from
// readLoop — which exists only on a client that owns a transport, the same
// constructor that installs peerWoke. Armed on a client without one, the
// wait can never be woken: it burns both deliveries' bounds and then calls
// a peer stuck that was never asked anything it could answer.
//
// The cost was not a wrong answer but an invisible one. Every hand-driven
// ACP client in the suite parked for 2x120s inside Prompt, the package
// blew its own 10m timeout, and the only symptom was `make gate` dying on
// a panic that named a select statement. This test fails in seconds and
// says which invariant broke.
func TestCursorPromptWithoutWakePathDoesNotPark(t *testing.T) {
	// An hour: if the watch arms here at all, the difference between the
	// fix and the bug is not a slow test, it is a hung one.
	shortenCursorSilenceBound(t, time.Hour)

	c := &cursorACPClient{sessionID: "s1", stdin: discardWrite{}, onEvent: func(Event) {}}
	done := make(chan error, 1)
	go func() { done <- c.Prompt("the opening brief") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt on an unwatchable client = %v, want a plain write", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt parked on a client nothing can wake: the silence watch armed " +
			"without a wake path, so its verdict was fixed before the wait began")
	}
}

// The same invariant one layer down, so a regression is attributed to the
// wait rather than to whichever caller happened to park on it.
func TestAwaitPeerActivityWithoutWakePathReturnsAndSaysWhy(t *testing.T) {
	c := &cursorACPClient{}
	start := time.Now()
	out := c.awaitPeerActivity(0, time.Hour)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("awaitPeerActivity sat for %v on a client with no wake channel", elapsed)
	}
	if out.spoke {
		t.Fatalf("outcome = %+v, want spoke=false: nothing ever sent anything", out)
	}
	if !strings.Contains(out.String(), "no inbound message") {
		t.Errorf("outcome %q does not say what the wait was watching for", out)
	}
}

// A wait that IS woken must report the peer spoke, so the two faults stay
// distinguishable in the log and the caller does not re-deliver a brief to
// a peer that already took it.
func TestAwaitPeerActivityReportsPeerSpoke(t *testing.T) {
	c := &cursorACPClient{peerWoke: make(chan struct{}, 1)}
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.notePeerActivity()
	}()
	out := c.awaitPeerActivity(0, 5*time.Second)
	if !out.spoke {
		t.Fatalf("outcome = %+v, want spoke=true after notePeerActivity", out)
	}
	if out.observed != 1 {
		t.Errorf("observed = %d, want 1 inbound message", out.observed)
	}
	if !strings.Contains(out.String(), "peer spoke") {
		t.Errorf("outcome %q does not name the peer's activity", out)
	}
}

// The third of the three faults the outcome exists to separate. 🎯T83's
// TestCursorStuckPromptWaitEndsWhenTransportDies already proves the wait
// ENDS when the transport dies, but it discards the outcome — so nothing
// asserted that a dead peer reads as dead rather than as a silent one.
// That distinction is the whole reason the wait reports a struct instead
// of a bool: silence means re-deliver, a closed transport means the
// caller's own error path.
func TestAwaitPeerActivityReportsTransportClosed(t *testing.T) {
	c := &cursorACPClient{peerWoke: make(chan struct{}, 1)}
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.wakePromptWaiters()
	}()
	out := c.awaitPeerActivity(0, time.Hour)
	if out.spoke || !out.closed {
		t.Fatalf("outcome = %+v, want spoke=false closed=true", out)
	}
	if got := out.String(); !strings.Contains(got, "transport closed") {
		t.Errorf("outcome %q does not distinguish a dead peer from a silent one", got)
	}
}

// 🎯T91's second half: when a real peer genuinely goes silent, the error
// says what was waited for. The old text gave a session and a duration,
// which left the reader unable to tell a swallowed delivery from a dead
// transport without going back to the wire.
func TestCursorStuckPromptErrorNamesBothDeliveries(t *testing.T) {
	shortenCursorSilenceBound(t, 150*time.Millisecond)
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_WITHHOLD", "session/prompt")

	agent, err := Start(Config{Provider: ProviderCursor, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	err = agent.Send("the opening brief")
	if !errors.Is(err, ErrCursorPromptStuck) {
		t.Fatalf("Send err = %v, want errors.Is ErrCursorPromptStuck", err)
	}
	msg := err.Error()
	for _, want := range []string{"two deliveries", "no inbound message"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q: it does not report what the wait observed", msg, want)
		}
	}
	// Both prompt ids, so the reader can find each delivery on the wire.
	var ids int
	for i := 1; i <= 8; i++ {
		if strings.Contains(msg, "prompt "+strconv.Itoa(i)+":") {
			ids++
		}
	}
	if ids != 2 {
		t.Errorf("error %q names %d prompt ids, want both deliveries", msg, ids)
	}
}

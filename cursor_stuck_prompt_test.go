// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cursorHermeticPeerBound is the silence bound to give a fake peer that
// has to ANSWER inside it, rather than one whose silence is the point.
//
// The shipped bound carries a measured margin: 120s against a worst
// observed first inbound message of 18.3s, ~6.5x (cursor_acp.go). A
// hermetic test that keeps the mechanism and throws the ratio away is
// not a scaled model of the product — it is a tighter experiment that
// fails for a reason the product would not, and its RED is the host's
// scheduler rather than the code's behaviour.
//
// 🎯T92 is the specimen. This file shortened the bound to 400ms and the
// failing run observed 402ms: a margin of 1.0x, on a host at load 122.
// The package went RED, `make gate` lost its citation, and the defect
// the run had actually found took a second reading to see.
//
// 402ms is the worst turnaround ever observed from the python fake, and
// it was observed under deliberate saturation. This is 6.5x of that,
// rounded up — the product's own ratio, applied to the fake's own
// measurement.
const cursorHermeticPeerBound = 3 * time.Second

// shortenCursorSilenceBound makes the 🎯T83 bound testable. The shipped
// value is measured against real mints (~6.5x the worst observed first
// response) and no hermetic test should sit through it.
//
// A test whose peer must stay SILENT for the bound may pass any small
// value: no scheduler can make silence arrive late. A test whose peer
// must SPEAK inside the bound passes cursorHermeticPeerBound, or states
// its own margin over what it asked the peer to do.
func shortenCursorSilenceBound(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorPromptSilenceBound
	cursorPromptSilenceBound = d
	t.Cleanup(func() { cursorPromptSilenceBound = prev })
}

// A peer that accepts session/new and then never answers session/prompt is
// the shape 🎯T83 names. Before the fix, Send returned nil, the prompt
// stack pinned, and every later Send reported ErrTurnInFlight forever — a
// seat that looks alive and busy and never takes work.
func TestCursorStuckFirstPromptFailsTypedAndLeavesSeatUsable(t *testing.T) {
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
	if err == nil {
		t.Fatal("a prompt the peer never answered reported success; the seat would pin behind it")
	}
	if !errors.Is(err, ErrCursorPromptStuck) {
		t.Fatalf("Send err = %v, want errors.Is ErrCursorPromptStuck", err)
	}
	if errors.Is(err, ErrTurnInFlight) {
		t.Fatal("a stuck mint must not report itself as busy: that reading is what forced kill-and-remint")
	}
	if !strings.Contains(err.Error(), agent.SessionID()) {
		t.Errorf("error %q does not name the stuck session %q", err, agent.SessionID())
	}

	// The acceptance's second half: a state a retry can use. The seat must
	// not still be holding a turn that never began.
	if agent.PromptInFlight() {
		t.Fatal("seat still reports a turn in flight; a retry would get ErrTurnInFlight, which is the pin")
	}
	if err := agent.Send("a retry the consumer makes in place"); !errors.Is(err, ErrCursorPromptStuck) {
		t.Fatalf("retry err = %v, want the same typed error (never ErrTurnInFlight)", err)
	}
}

// The bound must survive a slow-but-healthy peer, which is the failure
// mode a guessed timeout would have introduced: real mints take 14-18s to
// say their first word, so anything that treats slowness as stuckness
// breaks every healthy Cursor seat.
func TestCursorSlowFirstPromptIsNotStuck(t *testing.T) {
	shortenCursorSilenceBound(t, 5*time.Second)
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_PROMPT_DELAY_MS", "600")

	agent, err := Start(Config{Provider: ProviderCursor, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	start := time.Now()
	if err := agent.Send("Reply with exactly: pong"); err != nil {
		t.Fatalf("a peer that answered in 600ms was called stuck: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("Send returned in %v, before the peer could have answered — the wait is not observing anything", elapsed)
	}
	text, err := agent.WaitForResponse(t.Context())
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("reply %q, want pong", text)
	}
}

// Recovery is what separates this from a bare timeout: a peer that drops
// the first delivery and answers the re-issue must end up working, with no
// consumer-side lifecycle churn at all.
//
// The peer is silent on the first delivery by construction, so the first
// wait's verdict cannot depend on the host. The second one can — the
// fake has to answer the re-issue inside the bound — so it gets the
// product's own margin rather than the 400ms that made this test 🎯T92's
// specimen.
func TestCursorStuckFirstPromptRecoversOnReissue(t *testing.T) {
	shortenCursorSilenceBound(t, cursorHermeticPeerBound)
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_SWALLOW_FIRST_PROMPT", "1")

	agent, err := Start(Config{Provider: ProviderCursor, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if err := agent.Send("Reply with exactly: pong"); err != nil {
		t.Fatalf("Send after an in-harness re-issue: %v", err)
	}
	text, err := agent.WaitForResponse(t.Context())
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("reply %q, want pong", text)
	}
}

// 🎯T92 at the seat's own surface, through Start / Send / WaitForResponse
// and a real subprocess peer — the path the piped-transport differential
// in cursor_reissue_test.go cannot reach.
//
// The peer here was slow, not deaf: it holds the opening delivery,
// waits for the harness to give up and re-deliver, and then answers the
// delivery the harness abandoned. Its cue is the re-delivery, which this
// test causes, so nothing in the stimulus is a stopwatch.
//
// Before the fix the abandoned id was dropped from the prompt stack, its
// result settled as acpPromptNotOurs, and no terminal event was ever
// published: WaitForResponse had the reply's text and nothing to stop
// waiting for. That is the indefinite park this target was escalated for.
func TestCursorSlowPeerAnswersTheAbandonedDeliveryAndTheCallerSeesIt(t *testing.T) {
	shortenCursorSilenceBound(t, cursorHermeticPeerBound)
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_ANSWER_ABANDONED", "1")

	agent, err := Start(Config{Provider: ProviderCursor, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if err := agent.Send("Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	text, err := agent.WaitForResponse(t.Context())
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("reply %q, want pong: the peer answered the delivery the harness gave up on, "+
			"and that answer is the turn's", text)
	}
}

// Only the opening prompt is watched. Later sends must stay a plain write,
// or every Send on every Cursor seat inherits the bound's latency.
func TestCursorSecondPromptIsNotWatched(t *testing.T) {
	shortenCursorSilenceBound(t, 30*time.Second)
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	agent, err := Start(Config{Provider: ProviderCursor, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if err := agent.Send("first"); err != nil {
		t.Fatalf("Send#1: %v", err)
	}
	if _, err := agent.WaitForResponse(t.Context()); err != nil {
		t.Fatalf("WaitForResponse#1: %v", err)
	}
	start := time.Now()
	if err := agent.Send("second"); err != nil {
		t.Fatalf("Send#2: %v", err)
	}
	// A watched send would block until the peer spoke; an unwatched one
	// returns as soon as the line is written.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Send#2 took %v — the post-mint path is still paying the stuck-prompt wait", elapsed)
	}
}

// A dead transport must end the wait, not sit out the full bound.
func TestCursorStuckPromptWaitEndsWhenTransportDies(t *testing.T) {
	shortenCursorSilenceBound(t, time.Hour)
	c := &cursorACPClient{peerWoke: make(chan struct{}, 1)}
	var done atomic.Bool
	go func() {
		c.awaitPeerActivity(0, cursorPromptSilenceBound)
		done.Store(true)
	}()
	time.Sleep(50 * time.Millisecond)
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.wakePromptWaiters()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if done.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("awaitPeerActivity did not return when the transport closed")
}

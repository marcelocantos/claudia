// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 🎯T97 exemption, both constants below. Neither can outvote an assertion
// on a loaded host, because neither is the thing the test waits on.
//
// peerSpeaksFailsafe bounds a wait on the fake peer, whose answer IS the
// signal — it exists only so a peer that never answers fails this test
// instead of hanging the package, and it is ~15x the whole isolated run
// of the neighbouring Cursor tests.
//
// turnAlreadyOverFailsafe bounds a WaitForResponse call made only after
// the turn's terminal event has already been observed. On the fixed code
// that call has nothing left to wait for but waitSettleDuration (250ms),
// so 5s is 20x a bound the product itself sets; on the broken code it
// waits forever. No host is slow enough to turn the first into the
// second.
const (
	peerSpeaksFailsafe      = 15 * time.Second
	turnAlreadyOverFailsafe = 5 * time.Second
)

// awaitTerminalAssistant subscribes before the submit and reports when
// the turn's terminal assistant event has been published. That event, not
// a duration, is what the tests below decide on: once it has fired the
// whole turn is on the wire, and a WaitForResponse subscribing now sees
// none of it.
func awaitTerminalAssistant(t *testing.T, a *Agent) <-chan struct{} {
	t.Helper()
	done := make(chan struct{}, 1)
	token := a.SubscribeEvents(func(ev Event) {
		if ev.IsTerminalStop() {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})
	t.Cleanup(func() { a.UnsubscribeEvents(token) })
	return done
}

// 🎯T98, the park that stopped `make gate` finishing at all.
//
// Send on a Cursor opening prompt does not return until the peer has said
// something (🎯T83's silence watch), so by the time the caller reaches
// WaitForResponse the peer may already have streamed its reply AND its
// end_turn. WaitForResponse subscribed to future events only, so it
// waited for a turn that was over, and nothing was left that could end
// that wait.
//
// Alone the caller usually wins the race and the test it broke passed in
// 1.36s. In-suite it lost: TestCursorSlowFirstPromptIsNotStuck sat 9m12s
// in (*Agent).WaitForResponse — cursor_stuck_prompt_test.go:87 into
// agent.go:1864 — and took the root package's 10m alarm with it (gate
// 06df1f3b, load average 232-257).
func TestCursorWaitForResponseAfterTurnEndedStillReturnsTheReply(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	agent, err := Start(Config{Provider: ProviderCursor, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	terminal := awaitTerminalAssistant(t, agent)
	if err := agent.Send("Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-terminal:
	case <-time.After(peerSpeaksFailsafe):
		t.Fatal("the fake peer never published a terminal assistant event, so this test " +
			"cannot decide 🎯T98 either way")
	}

	ctx, cancel := context.WithTimeout(t.Context(), turnAlreadyOverFailsafe)
	defer cancel()
	text, err := agent.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("WaitForResponse on a turn that has already ended: %v — the caller "+
			"that submitted the turn cannot observe its own reply", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("reply %q, want pong", text)
	}
}

// The same invariant with no provider at all, so a regression is
// attributed to the Agent's turn bookkeeping rather than to whichever
// backend happened to answer quickly.
func TestWaitForResponseSeesTurnPublishedBeforeItSubscribed(t *testing.T) {
	a := &Agent{}
	a.beginTurn()
	a.PublishEvent(Event{Type: "assistant", Text: "pong", PreviewUpdate: PreviewUpdateAppend})
	a.PublishEvent(Event{Type: "assistant", StopReason: "end_turn"})

	ctx, cancel := context.WithTimeout(t.Context(), turnAlreadyOverFailsafe)
	defer cancel()
	text, err := a.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	if text != "pong" {
		t.Fatalf("text = %q, want pong", text)
	}
}

// A turn that failed before the caller subscribed must surface as that
// failure. 🎯T16's fail-loud promise had the same hole as the reply path:
// an error event published during a blocking submit was heard by nobody.
func TestWaitForResponseSeesTurnErrorPublishedBeforeItSubscribed(t *testing.T) {
	a := &Agent{}
	a.beginTurn()
	a.PublishEvent(Event{Type: "assistant", IsError: true, Text: "model_not_found"})

	ctx, cancel := context.WithTimeout(t.Context(), turnAlreadyOverFailsafe)
	defer cancel()
	if _, err := a.WaitForResponse(ctx); err == nil || !strings.Contains(err.Error(), "model_not_found") {
		t.Fatalf("err = %v, want the published failure", err)
	}
}

// The latch hands one turn to one waiter. A second wait with no submit
// between them has no turn to report and blocks for the next one, which
// is what WaitForResponse promised before 🎯T98 and still promises.
//
// 🎯T97 exemption: the 300ms here bounds a wait that must NOT return.
// A slower host only makes the assertion safer, so the clock cannot
// outvote it; the failure it reports is a return, not an expiry.
func TestWaitForResponseDoesNotReplayAConsumedTurn(t *testing.T) {
	a := &Agent{}
	a.beginTurn()
	a.PublishEvent(Event{Type: "assistant", Text: "pong", PreviewUpdate: PreviewUpdateAppend})
	a.PublishEvent(Event{Type: "assistant", StopReason: "end_turn"})

	first, cancelFirst := context.WithTimeout(t.Context(), turnAlreadyOverFailsafe)
	defer cancelFirst()
	if _, err := a.WaitForResponse(first); err != nil {
		t.Fatalf("first WaitForResponse: %v", err)
	}

	second, cancelSecond := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancelSecond()
	if text, err := a.WaitForResponse(second); err == nil {
		t.Fatalf("the second wait returned %q, the turn the first one already took", text)
	}
}

// Events published before any submit are not this caller's turn. A
// resumed session replays the previous conversation, and the common
// `go WaitForResponse(); Send()` shape must not be handed that replay as
// the answer to a prompt nobody has sent yet.
//
// 🎯T97 exemption: as above — the 300ms bounds a wait that must not
// return, so a slow host can only strengthen the assertion.
func TestWaitForResponseIgnoresEventsFromBeforeTheSubmit(t *testing.T) {
	a := &Agent{}
	a.PublishEvent(Event{Type: "assistant", Text: "an old reply", StopReason: "end_turn"})

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if text, err := a.WaitForResponse(ctx); err == nil {
		t.Fatalf("WaitForResponse returned %q, published before the submit", text)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"strings"
)

// turnLatch is what [Agent.publishEvent] has already seen of the turn
// the caller most recently submitted.
//
// [Agent.WaitForResponse] subscribes to events, and a subscription only
// hears the future. That is sound while a submit returns before its turn
// can answer — the caller is subscribed long before the first token. It
// is not sound for a backend whose submit blocks until the peer has
// spoken: 🎯T83's Cursor silence watch holds Send open until the peer
// says something, so a fake (or a fast real peer) can stream its reply
// and its end_turn while Send is still unwinding. The caller then
// subscribes to a turn that is already over and waits for an event that
// has been and gone.
//
// The cost was a hung package rather than a wrong answer:
// TestCursorSlowFirstPromptIsNotStuck passed in 1.36s alone and sat
// 9m12s in WaitForResponse in-suite, taking the root package's 10m alarm
// and `make gate` with it (🎯T98; gate 06df1f3b).
//
// 🎯T96's silence bound now ends such a wait, which makes it a reported
// failure instead of a hung suite — but a reported failure is still the
// wrong answer here, because the reply DID arrive. The two fixes are
// complementary: 🎯T96 guarantees the wait ends, this one guarantees it
// ends with the turn.
//
// So the turn is recorded as it is published, and a wait that arrives
// late is handed what it missed. The latch is ARMED by the submit, which
// is what keeps it from handing back somebody else's conversation: a
// resumed session replays old assistant messages, and those must not
// satisfy a caller waiting on a prompt nobody has sent yet.
//
// It holds at most one turn's text, and only until the next submit or
// the wait that takes it — the same text a WaitForResponse caller would
// be accumulating anyway. It is not capped: a cap would silently
// truncate a reply, a worse failure than the memory it saves.
type turnLatch struct {
	// armed is set by beginTurn and cleared when a wait takes the turn.
	// Unarmed, publishEvent records nothing.
	armed bool
	// text accumulates exactly as WaitForResponse's own subscriber does,
	// via appendTurnText, so a delta stream is not corrupted by newlines
	// (🎯T79). A pointer so resetting the latch never copies a used
	// strings.Builder.
	text     *strings.Builder
	terminal bool
	err      error
}

// turnSoFar is a latch handed to one waiter. Empty when no submit has
// happened, or when an earlier wait already took the turn.
type turnSoFar struct {
	text     string
	terminal bool
	err      error
}

// beginTurn arms the latch for a newly submitted turn, discarding
// whatever the previous one left behind.
func (a *Agent) beginTurn() {
	a.mu.Lock()
	a.turn = turnLatch{armed: true, text: &strings.Builder{}}
	a.mu.Unlock()
}

// recordTurnEventLocked folds ev into the armed turn. It mirrors
// WaitForResponse's subscriber exactly; the two must agree, or a waiter
// that arrives late gets a different answer from one that arrived early.
func (a *Agent) recordTurnEventLocked(ev Event) {
	if !a.turn.armed {
		return
	}
	if ev.IsError {
		if a.turn.err == nil {
			msg := strings.TrimSpace(ev.Text)
			if msg == "" {
				msg = "agent turn failed"
			}
			a.turn.err = errors.New(msg)
		}
		return
	}
	if ev.Type != "assistant" {
		return
	}
	if a.turn.text == nil {
		a.turn.text = &strings.Builder{}
	}
	appendTurnText(a.turn.text, ev)
	if ev.IsTerminalStop() {
		a.turn.terminal = true
	}
}

// takeTurnLocked hands the armed turn to one waiter and disarms the
// latch, so a second wait with no submit between them blocks for a new
// turn rather than being told the old one again.
func (a *Agent) takeTurnLocked() turnSoFar {
	if !a.turn.armed {
		return turnSoFar{}
	}
	out := turnSoFar{terminal: a.turn.terminal, err: a.turn.err}
	if a.turn.text != nil {
		out.text = a.turn.text.String()
	}
	a.turn = turnLatch{}
	return out
}

// subscribeEventsTakingTurn registers fn and, in the same critical
// section, hands the turn recorded so far to seed.
//
// The atomicity is the point. publishEvent copies the subscriber list
// under a.mu and calls the copies after releasing it, so an event either
// reaches seed (recorded before fn was registered) or fn (published
// after), never both and never neither — and seed always runs before
// fn's first call, so the turn's head cannot land behind its tail.
func (a *Agent) subscribeEventsTakingTurn(fn EventFunc, seed func(turnSoFar)) int64 {
	id := nextEventSubID.Add(1)
	a.mu.Lock()
	if a.eventSubs == nil {
		a.eventSubs = make(map[int64]EventFunc)
	}
	a.eventSubs[id] = fn
	seed(a.takeTurnLocked())
	hook := a.onSubscribe
	a.onSubscribe = nil
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return id
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
	"time"
)

// 🎯T602. For a tmux-backed session the window IS the agent. Alive()
// returned a cached flag, so when the overseer's window vanished on
// 2026-08-31 the daemon went on believing the process alive: its converge
// loop chose "unstick" over "launch" and tried to send Escape to a window
// tmux had already forgotten, every 90 seconds, forever. It could not
// self-heal, because the fact that would have triggered a relaunch was the
// one it had wrong.
func TestAliveIsFalseWhenTheWindowIsGone(t *testing.T) {
	// A window id the tmux server has certainly never issued.
	a := &Agent{alive: true, tmuxWindowID: "@999999", windowAliveFn: func(string) bool { return false }}
	if a.Alive() {
		t.Fatal("an agent whose tmux window does not exist reported itself alive")
	}
	// And it latches: the flag itself is now false, so later callers get
	// the truth without paying for another probe.
	a.mu.Lock()
	flag := a.alive
	a.mu.Unlock()
	if flag {
		t.Fatal("a dead window did not latch the alive flag")
	}
}

// A stopped agent stays stopped without consulting tmux at all — Stop is
// authoritative and must not be second-guessed by a probe.
func TestAliveStaysFalseForAStoppedAgent(t *testing.T) {
	a := &Agent{alive: false, tmuxWindowID: "@1"}
	if a.Alive() {
		t.Fatal("a stopped agent reported alive")
	}
}

// Non-tmux agents (Grok connect, Codex app-server) have no window, and
// their flag remains the whole answer. Probing tmux for them would be
// both wrong and a needless exec.
func TestAliveIsUnchangedForAgentsWithoutAWindow(t *testing.T) {
	a := &Agent{alive: true}
	if !a.Alive() {
		t.Fatal("an agent with no tmux window was killed by the window check")
	}
}

// A synthetic WindowID is not a claim that tmux knows about it. Only a
// backend that really made a window supplies the probe; without one the
// old flag-only answer stands.
//
// This is not a nicety. The first version of this fix probed real tmux
// whenever tmuxWindowID was set, so every hermetic fixture became
// permanently not-alive and the suite went from 31s to a timeout: tests
// waiting for an agent to come up waited forever.
func TestAFixtureWindowIsNotProbed(t *testing.T) {
	a := &Agent{alive: true, tmuxWindowID: "@fixture-not-a-real-window"}
	if !a.Alive() {
		t.Fatal("a fixture with no probe was declared dead by the window check")
	}
}

// The probe is cached: Alive sits on the converge loop's path, once per
// agent every few seconds, and each miss is a tmux exec.
func TestTheWindowProbeIsCached(t *testing.T) {
	a := &Agent{alive: true, tmuxWindowID: "@999999", windowAliveFn: func(string) bool { return false }}
	_ = a.Alive() // first call probes and latches false

	// Pretend the flag is true again but the cache is fresh: the cached
	// answer must be used rather than a second probe.
	a.mu.Lock()
	a.alive = true
	a.windowCheckAt = time.Now()
	a.windowCheckOK = true
	a.mu.Unlock()
	if !a.Alive() {
		t.Fatal("a fresh cached answer was ignored")
	}

	// Expire the cache and the truth reasserts itself.
	a.mu.Lock()
	a.windowCheckAt = time.Now().Add(-windowCheckTTL - time.Second)
	a.mu.Unlock()
	if a.Alive() {
		t.Fatal("an expired cache kept reporting a dead window as alive")
	}
}

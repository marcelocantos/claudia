// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package broker holds the claudia lifecycle-broker daemon internals.
//
// Everything here is built oracle-first (see docs/broker-oracles.md): the
// broker's policy loops — AIMD concurrency control, idle reaping, warm-pool
// draining, preemption — must be verifiable by deterministic tests, never by
// watching a live daemon and eyeballing 429s and timers (a class-4 dynamical
// signal worth ~1 bit per round-trip). Two seams make that possible: the Clock
// below (all timing decisions read time through it) and a behavioural fake
// `claude` in the brokertest package (429s, canned JSONL, readiness on demand).
// A build guard (policy_guard_test.go) fails the build if any policy file reads
// the wall clock directly.
//
// This package is internal: it adds nothing to the public API of
// github.com/marcelocantos/claudia, so the broker work is strictly additive and
// does not disturb the pre-1.0 shakeout contract.
package broker

import (
	"sync"
	"time"
)

// Clock is the broker's sole source of time. Every policy decision that depends
// on the passage of time — idle-reap TTLs, AIMD recovery windows, pool-drain
// timers — reads it through this interface so tests can drive those decisions
// deterministically with a ManualClock instead of waiting on the wall clock.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed,
	// analogous to time.After. A non-positive d fires immediately.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the production Clock backed by the real wall clock.
type SystemClock struct{}

// Now reports the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// After delegates to time.After.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// ManualClock is a Clock whose time advances only when a test calls Advance.
// Timers returned by After fire when Advance crosses their deadline, so a test
// can exercise TTL and recovery logic in microseconds with no real sleeping and
// no flakiness. It is safe for concurrent use.
type ManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []manualTimer
}

type manualTimer struct {
	deadline time.Time
	ch       chan time.Time
}

// NewManualClock returns a ManualClock started at t.
func NewManualClock(t time.Time) *ManualClock { return &ManualClock{now: t} }

// Now reports the manual clock's current time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that fires when the clock is advanced to or past
// now+d. A non-positive d fires immediately.
func (c *ManualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, manualTimer{deadline: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock forward by d and fires every timer whose deadline the
// new time reaches or passes.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if c.now.Before(t.deadline) {
			kept = append(kept, t)
		} else {
			t.ch <- t.deadline
		}
	}
	c.timers = kept
}

// Compile-time proof that both clocks satisfy the interface.
var (
	_ Clock = SystemClock{}
	_ Clock = (*ManualClock)(nil)
)

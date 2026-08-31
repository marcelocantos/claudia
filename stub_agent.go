// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

// NewStubAgent returns an Agent that reports itself alive and delivers Send
// to fn, without a process, a TUI, or a tmux window behind it.
//
// It exists because Alive() reads unexported state, so a dependent package
// had no way to build a server whose overseer is up. jevons gates owner
// sends on `proc == nil || !proc.Alive()` (its 🎯T545 rule: a down overseer
// is a nack, not a silent enqueue), and its tests faked the overseer with a
// zero &claudia.Agent{} — which reports NOT alive. Four of them went red on
// master the day that gate landed and stayed there, because the only way to
// satisfy the gate was to start a real agent.
//
// A stub is the honest seam for that: the test drives the same Send path the
// product does, rather than a parallel one that can drift away from it.
//
// fn may be nil, in which case Send succeeds and discards the message.
func NewStubAgent(fn func(string) error) *Agent {
	ready := make(chan struct{})
	close(ready) // Send waits on this; a stub is ready immediately.
	a := &Agent{alive: true, ready: ready}
	a.ops = agentOps{send: func(_ *Agent, msg string) error {
		if fn == nil {
			return nil
		}
		return fn(msg)
	}}
	return a
}

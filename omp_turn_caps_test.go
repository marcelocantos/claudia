// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"sync/atomic"
	"testing"
)

// A sidecar seat runs under its seat id, which the provider turn-caps table
// does not name. It must still report the steer and interrupt it has wired,
// or a host escalating an owner message to a busy seat holds it behind the
// turn instead (jevons J35, 2026-10-05).
func TestSidecarSeatReportsItCanSteer(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "tok", &refreshes, "tok")
	agent, _ := startT141Seat(t, s)

	caps := agent.TurnCaps()
	if !caps.CanSteer || caps.SteerPolicy != SteerBreakpoint || !caps.CanInterrupt {
		t.Fatalf("sidecar seat (%s) turn caps = %+v, want steer at breakpoint and interrupt", agent.Provider(), caps)
	}
}

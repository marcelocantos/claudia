// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package brokerguard holds the ratchet that keeps hermetic test packages off
// a claudia broker running on the machine.
//
// The library consults the default broker socket (~/.local/state/claudia, or
// $XDG_STATE_HOME/claudia) whenever one is listening and CLAUDIA_NO_BROKER is
// unset (🎯T3). A test binary that links the broker client can therefore hand
// fixture seats to the owner's live broker. On 2026-10-19 a plain
// `go test ./...` did exactly that: controlplane's reconcile tests had no
// TestMain, `Registry.Launch` asked the running dev broker for seat-a, and its
// sidecar refused "xai-oauth/m". `make gate` sets CLAUDIA_NO_BROKER=1 for
// its own run (internal/ownergate), but a direct `go test` gets no such
// protection.
//
// The set is computed, not hand-listed. Every test binary whose dependencies
// include the broker client (internal/broker) must have a TestMain that
// switches the consult off before m.Run, as the root and daemon packages
// do. A test that wants a broker starts its own on a temp socket and turns the
// consult back on with t.Setenv(broker.NoBrokerEnv, "").
package brokerguard

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"github.com/marcelocantos/claudia/internal/broker"
	"testing"
)

// Production grant-by-name release must interrupt before it removes the
// connection; closing a handle alone does not stop a sidecar model stream.
func TestT238ReleaseStopsInFlightTurnBeforeRegrant(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	c := rawSeat(t, f.sock, "release-race")
	sent := rawCall(t, c, &broker.Request{ID: "send", Type: broker.TypeSend,
		Send: &broker.SendRequest{Name: "release-race", Text: "delayed old stream"}})
	if sent.Type != broker.TypeSent {
		t.Fatalf("send: %+v", sent)
	}
	old := f.seat(0)
	if !old.inFlight.Load() {
		t.Fatal("fixture did not start old turn")
	}
	released := rawCall(t, c, &broker.Request{ID: "release", Type: broker.TypeRelease,
		Release: &broker.ReleaseRequest{Name: "release-race", Disposition: broker.DispositionStop}})
	if released.Type != broker.TypeReleased {
		t.Fatalf("release: %+v", released)
	}
	interrupts, _, stops := old.counts()
	if interrupts != 1 || stops != 1 || old.inFlight.Load() {
		t.Fatalf("released live seat: interrupts=%d stops=%d inFlight=%t", interrupts, stops, old.inFlight.Load())
	}
	// The backend has no old work to bill when the next grant starts.
	c2 := rawSeat(t, f.sock, "release-race")
	_ = c2
	if f.seatCount() != 2 || old.inFlight.Load() {
		t.Fatal("old provider survived next grant")
	}
}

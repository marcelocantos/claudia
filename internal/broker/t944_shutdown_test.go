// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import "testing"

// Jevons 🎯T944: a server stopped on purpose tells every served connection
// so before closing it, so a host can tell a planned restart from a crash
// and alarm only on the crash.
func TestT944ShutdownTellsEveryConnection(t *testing.T) {
	path, srv := startTestServer(t)
	conns := []*Conn{dialTest(t, path), dialTest(t, path)}
	// A round trip each, so both connections are being served.
	for _, c := range conns {
		if resp := roundTrip(t, c, &Request{ID: "s", Type: TypeStatus, Status: &StatusRequest{}}); resp.Type != TypeStatusResult {
			t.Fatalf("status: %+v", resp)
		}
	}
	if err := srv.Shutdown(ShutdownPlanned); err != nil {
		t.Fatal(err)
	}
	for i, c := range conns {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatalf("conn %d: no goodbye before close: %v", i, err)
		}
		if resp.Type != TypeEvent || resp.Event == nil || resp.Event.Kind != EventShutdown || resp.Event.Detail != ShutdownPlanned {
			t.Fatalf("conn %d: got %+v, want the planned shutdown event", i, resp)
		}
		if _, err := c.ReadResponse(); err == nil {
			t.Fatalf("conn %d still open after the goodbye", i)
		}
	}
}

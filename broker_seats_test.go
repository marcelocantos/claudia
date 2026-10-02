// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

type grantsHandler struct {
	grants   []broker.GrantStatus
	released chan *broker.ReleaseRequest
}

func (h grantsHandler) HandleRequest(c *broker.ClientConn, req *broker.Request) bool {
	switch req.Type {
	case broker.TypeGrants:
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGrantsResult, Grants: &broker.GrantsResponse{Grants: h.grants}})
		return true
	case broker.TypeRelease:
		h.released <- req.Release
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeReleased,
			Released: &broker.ReleaseResponse{Name: req.Release.Name, Disposition: req.Release.Disposition}})
		return true
	}
	return false
}

func (grantsHandler) ConnClosed(*broker.ClientConn) {}

func startGrantsBroker(t *testing.T, h grantsHandler) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(broker.SocketPathEnv, filepath.Join(dir, "b.sock"))
	t.Setenv(broker.NoBrokerEnv, "")
	path, err := broker.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := broker.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := broker.Serve(&broker.ServeArgs{Listener: ln, Handler: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
}

// jevons 🎯T984: a consumer reads the broker's own view of every seat.
func TestBrokerSeatsListsTheBrokersView(t *testing.T) {
	startGrantsBroker(t, grantsHandler{grants: []broker.GrantStatus{
		{Name: "held", Provider: "anthropic", Owned: true, Alive: true},
		{Name: "orphan", Provider: "anthropic", Owned: false, Alive: true},
		{Name: "down", Provider: "cursor"},
	}})

	seats, err := BrokerSeats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []BrokerSeat{
		{Name: "held", Provider: "anthropic", Owned: true, Alive: true},
		{Name: "orphan", Provider: "anthropic", Alive: true},
		{Name: "down", Provider: "cursor"},
	}
	if len(seats) != len(want) {
		t.Fatalf("seats = %+v", seats)
	}
	for i := range want {
		if seats[i] != want[i] {
			t.Fatalf("seat %d = %+v, want %+v", i, seats[i], want[i])
		}
	}
}

// jevons 🎯T984: a seat resumed against its consumer's intent is stopped by
// name, with the stop disposition.
func TestStopBrokerSeatReleasesWithStop(t *testing.T) {
	released := make(chan *broker.ReleaseRequest, 1)
	startGrantsBroker(t, grantsHandler{released: released})
	if err := StopBrokerSeat(context.Background(), "orphan"); err != nil {
		t.Fatal(err)
	}
	r := <-released
	if r.Name != "orphan" || r.Disposition != broker.DispositionStop || r.Force {
		t.Fatalf("release = %+v, want a plain stop of orphan", r)
	}
}

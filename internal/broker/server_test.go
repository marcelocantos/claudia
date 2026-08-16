// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "b.sock")
}

func startTestServer(t *testing.T) (string, *Server) {
	t.Helper()
	sock := shortSocket(t)
	t.Setenv(SocketPathEnv, sock)
	path, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(&ServeArgs{Listener: ln, Clock: NewManualClock(goldenAt)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return path, srv
}

func dialTest(t *testing.T, path string) *Conn {
	t.Helper()
	c, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func roundTrip(t *testing.T, c *Conn, req *Request) *Response {
	t.Helper()
	if err := c.WriteRequest(req); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	resp, err := c.ReadResponse()
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	return resp
}

// TestSpawnReleaseOverRealSocket is the class-1 integration half of 🎯T2.1:
// a real Unix socket, newline-delimited JSON, spawn then release.
func TestSpawnReleaseOverRealSocket(t *testing.T) {
	path, _ := startTestServer(t)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("socket was not created at %s: %v", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s exists but is not a socket", path)
	}

	c := dialTest(t, path)
	spawned := roundTrip(t, c, &Request{
		ID:   "r1",
		Type: TypeSpawn,
		Spawn: &SpawnRequest{
			Mode:    ModeSession,
			WorkDir: "/w",
			Model:   "claude-opus-5",
		},
	})
	if spawned.Type != TypeSpawned || spawned.Spawned == nil {
		t.Fatalf("spawn: %+v", spawned)
	}
	if spawned.ID != "r1" {
		t.Errorf("spawned id = %q, want r1", spawned.ID)
	}
	if spawned.Spawned.SessionID == "" {
		t.Fatal("spawned session_id is empty")
	}
	if spawned.Spawned.PID == 0 {
		t.Error("spawned pid is 0")
	}
	if spawned.Spawned.Warm {
		t.Error("spawned warm = true; T2.1 has no pool")
	}

	released := roundTrip(t, c, &Request{
		ID:   "r2",
		Type: TypeRelease,
		Release: &ReleaseRequest{
			SessionID:   spawned.Spawned.SessionID,
			Disposition: DispositionStop,
		},
	})
	if released.Type != TypeReleased || released.Released == nil {
		t.Fatalf("release: %+v", released)
	}
	if released.Released.SessionID != spawned.Spawned.SessionID {
		t.Errorf("released session = %q, want %q", released.Released.SessionID, spawned.Spawned.SessionID)
	}
	if released.Released.Disposition != DispositionStop {
		t.Errorf("disposition = %q, want %q", released.Released.Disposition, DispositionStop)
	}
}

func TestStatusAndTailOverRealSocket(t *testing.T) {
	path, _ := startTestServer(t)
	c := dialTest(t, path)

	empty := roundTrip(t, c, &Request{ID: "st0", Type: TypeStatus, Status: &StatusRequest{}})
	if empty.Type != TypeStatusResult || empty.Status == nil {
		t.Fatalf("status empty: %+v", empty)
	}
	if empty.Status.ProtocolVersion != Version || empty.Status.ActiveAgents != 0 || len(empty.Status.Sessions) != 0 {
		t.Errorf("empty snapshot = %+v", empty.Status)
	}

	spawned := roundTrip(t, c, &Request{
		ID:    "sp",
		Type:  TypeSpawn,
		Spawn: &SpawnRequest{Mode: ModeTask, WorkDir: "/repo", Intent: IntentBatch},
	})
	if spawned.Spawned == nil {
		t.Fatalf("spawn: %+v", spawned)
	}

	one := roundTrip(t, c, &Request{ID: "st1", Type: TypeStatus, Status: &StatusRequest{}})
	if one.Status == nil || one.Status.ActiveAgents != 1 || len(one.Status.Sessions) != 1 {
		t.Fatalf("status one: %+v", one.Status)
	}
	got := one.Status.Sessions[0]
	if got.SessionID != spawned.Spawned.SessionID || got.Mode != ModeTask || got.WorkDir != "/repo" || got.Intent != IntentBatch {
		t.Errorf("session snapshot = %+v", got)
	}

	tail := dialTest(t, path)
	ack := roundTrip(t, tail, &Request{ID: "tl", Type: TypeTail, Tail: &TailRequest{}})
	if ack.Type != TypeTailing {
		t.Fatalf("tail ack: %+v", ack)
	}

	_ = roundTrip(t, c, &Request{
		ID:    "sp2",
		Type:  TypeSpawn,
		Spawn: &SpawnRequest{Mode: ModeSession, WorkDir: "/w2"},
	})
	ev, err := tail.ReadResponse()
	if err != nil {
		t.Fatalf("tail event: %v", err)
	}
	if ev.Type != TypeEvent || ev.Event == nil || ev.Event.Kind != EventSpawn {
		t.Errorf("tail event = %+v", ev)
	}
}

func TestReleaseUnknownAndNotOwner(t *testing.T) {
	path, _ := startTestServer(t)
	owner := dialTest(t, path)
	other := dialTest(t, path)

	spawned := roundTrip(t, owner, &Request{
		ID:    "sp",
		Type:  TypeSpawn,
		Spawn: &SpawnRequest{Mode: ModeSession, WorkDir: "/w"},
	})
	sid := spawned.Spawned.SessionID

	unknown := roundTrip(t, owner, &Request{
		ID:      "u",
		Type:    TypeRelease,
		Release: &ReleaseRequest{SessionID: "no-such", Disposition: DispositionStop},
	})
	if unknown.Error == nil || unknown.Error.Code != CodeUnknownSession {
		t.Errorf("unknown: %+v", unknown)
	}

	stolen := roundTrip(t, other, &Request{
		ID:      "n",
		Type:    TypeRelease,
		Release: &ReleaseRequest{SessionID: sid, Disposition: DispositionStop},
	})
	if stolen.Error == nil || stolen.Error.Code != CodeNotOwner {
		t.Errorf("not owner: %+v", stolen)
	}

	reuse := roundTrip(t, owner, &Request{
		ID:      "r",
		Type:    TypeRelease,
		Release: &ReleaseRequest{SessionID: sid, Disposition: DispositionReuse},
	})
	if reuse.Error == nil || reuse.Error.Code != CodeUnsupportedValue {
		t.Errorf("reuse: %+v", reuse)
	}
}

func TestListenRefusesALiveBroker(t *testing.T) {
	path, _ := startTestServer(t)
	_, err := Listen(path)
	if err == nil {
		t.Fatal("Listen stole a live broker's socket")
	}
}

func TestServeRequiresListener(t *testing.T) {
	if _, err := Serve(nil); err == nil {
		t.Error("Serve(nil) succeeded")
	}
	if _, err := Serve(&ServeArgs{}); err == nil {
		t.Error("Serve with no Listener succeeded")
	}
}

func TestMalformedRequestIsTypedError(t *testing.T) {
	path, _ := startTestServer(t)
	c := dialTest(t, path)
	if err := c.writeLine([]byte(`{"v":1,"type":"teleport"}`)); err != nil {
		t.Fatal(err)
	}
	resp, err := c.ReadResponse()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != CodeUnknownType {
		t.Errorf("got %+v, want unknown_type", resp)
	}
	var pe *ProtocolError
	if !errors.As(resp.Error.Err(), &pe) {
		t.Fatalf("error body did not round-trip: %+v", resp.Error)
	}
}

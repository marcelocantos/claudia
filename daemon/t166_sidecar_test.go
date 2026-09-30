// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// 🎯T166: the broker's sidecar starts once the daemon holds its socket and
// before any seat resumes, so no seat lands on a sidecar an earlier broker
// left behind.
func TestT166SidecarStartsBeforeAnySeatResumes(t *testing.T) {
	f := newFixture(t)
	writeGrantsTable(t, f.state, []claudia.AgentDef{
		{Name: "held", WorkDir: t.TempDir(), SessionID: "sid-held", AutoStart: true},
	})
	var sidecar atomic.Bool
	var early atomic.Bool
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		if !sidecar.Load() {
			early.Store(true)
		}
		return f.startSeat(ctx, cfg)
	}
	opts := f.options(nil)
	opts.StartSidecar = func(context.Context) error {
		if _, err := os.Stat(f.sock); err != nil {
			t.Errorf("sidecar started before the daemon held its socket: %v", err)
		}
		sidecar.Store(true)
		return nil
	}
	f.bootWith(t, opts)
	waitFor(t, "seat resumed", func() bool { return f.seat(0) != nil })
	if early.Load() {
		t.Fatal("a seat resumed before the broker's sidecar started")
	}
}

// A broker that cannot replace a leftover sidecar does not serve: its seats
// would land on the leftover.
func TestT166SidecarFailureStopsTheBroker(t *testing.T) {
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume = true
	refused := errors.New("leftover sidecar will not stop")
	opts.StartSidecar = func(context.Context) error { return refused }
	d, err := New(opts)
	if err == nil {
		_ = d.Close()
		t.Fatal("New served with a sidecar it could not start")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("New = %v, want the sidecar's error", err)
	}
	// The socket is released for the next attempt.
	ln, err := broker.Listen(f.sock)
	if err != nil {
		t.Fatalf("socket still held after a refused start: %v", err)
	}
	ln.Close()
}

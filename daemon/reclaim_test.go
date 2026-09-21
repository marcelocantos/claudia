// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// detachedFixture boots a daemon with a tiny, held pump and starts a real
// consumer on seat name, then overflows the pump so the daemon detaches it.
func detachedFixture(t *testing.T, name string) (*fixture, *claudia.Agent, func()) {
	t.Helper()
	f := newFixture(t)
	gate, release := heldPump(t)
	opts := f.options(nil)
	opts.DisableResume, opts.pumpSize, opts.pumpGate = true, 2, gate
	f.bootWith(t, opts)
	t.Cleanup(release)
	a, err := claudia.Start(claudia.Config{Name: name, WorkDir: t.TempDir(), SessionID: "sid-" + name, TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	return f, a, release
}

// TestT124SendAfterDetachReclaimsAndSucceeds: the daemon detaches a consumer
// whose queue overflowed; the consumer's next Send is not refused as
// not_owner, the grant is owned again, and no second process was started.
// Removing the consumer's re-claim turns it red.
func TestT124SendAfterDetachReclaimsAndSucceeds(t *testing.T) {
	f, a, release := detachedFixture(t, "recover")
	proc := f.d.reg.Get("recover")
	for i := 0; i < 20; i++ {
		proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("t%d", i)})
	}
	waitFor(t, "detached", func() bool { return !f.owned("recover") })
	release()
	if err := a.Send("after the detach"); err != nil {
		t.Fatalf("Send after detach: %v", err)
	}
	if got := f.seat(0).sent(); len(got) != 1 || got[0] != "after the detach" {
		t.Fatalf("seat sends = %v", got)
	}
	waitFor(t, "owned again", func() bool { return f.owned("recover") })
	if f.seat(1) != nil {
		t.Fatal("re-claim started a second provider process")
	}
}

// TestT124SilentDetachIsRecoveredOnNotOwner: a daemon that detached without
// the message (an older daemon) still recovers, on the first not_owner.
func TestT124SilentDetachIsRecoveredOnNotOwner(t *testing.T) {
	f, a, _ := detachedFixture(t, "silent")
	f.d.mu.Lock()
	f.d.detachLocked(f.d.grants["silent"])
	f.d.mu.Unlock()
	if f.owned("silent") {
		t.Fatal("fixture did not detach")
	}
	if err := a.Send("after the silent detach"); err != nil {
		t.Fatalf("Send after silent detach: %v", err)
	}
	if !f.owned("silent") || len(f.seat(0).sent()) != 1 {
		t.Fatalf("owned=%v sends=%v", f.owned("silent"), f.seat(0).sent())
	}
}

// TestT124ReadoptOfDetachedSeatReclaims: a Registry whose handle the daemon
// detached must not return it unchanged from AdoptOrLaunch (the early return
// at the top of startHeld); the grant is owned again afterwards. Restoring
// that early return turns this red.
func TestT124ReadoptOfDetachedSeatReclaims(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	reg, err := claudia.NewRegistry(t.TempDir() + "/agents.json")
	if err != nil {
		t.Fatal(err)
	}
	def := claudia.AgentDef{Name: "readopt", WorkDir: t.TempDir(), SessionID: "sid-readopt", TermLogPath: "-"}
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Launch("readopt"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Stop("readopt") })
	f.d.mu.Lock()
	f.d.detachLocked(f.d.grants["readopt"])
	f.d.mu.Unlock()
	proc, err := reg.AdoptOrLaunch("readopt")
	if err != nil {
		t.Fatal(err)
	}
	if !f.owned("readopt") {
		t.Fatal("re-adopt returned the detached handle without re-claiming the grant")
	}
	if err := proc.Send("hello"); err != nil {
		t.Fatalf("Send after re-adopt: %v", err)
	}
	if f.seat(1) != nil {
		t.Fatal("re-adopt started a second provider process")
	}
}

// TestT124StatusShowsOwnerAndOperatorForceDetach: grants list names the
// holding connection and its peer pid; a third connection can clear the
// grant with a forced detach, the old owner is told, the seat process
// stays alive, and a new connection can then take the grant.
func TestT124StatusShowsOwnerAndOperatorForceDetach(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	owner := rawSeat(t, f.sock, "held")
	operator := dialRaw(t, f.sock)

	grants := func() broker.GrantStatus {
		resp := rawCall(t, operator, &broker.Request{ID: "g", Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}})
		for _, g := range resp.Grants.Grants {
			if g.Name == "held" {
				return g
			}
		}
		t.Fatal("grant not listed")
		return broker.GrantStatus{}
	}
	if g := grants(); !g.Owned || g.OwnerConn == 0 || g.OwnerPID != os.Getpid() {
		t.Fatalf("owned grant status = %+v, want owner conn and pid %d", g, os.Getpid())
	}

	resp := rawCall(t, operator, &broker.Request{ID: "r1", Type: broker.TypeRelease,
		Release: &broker.ReleaseRequest{Name: "held", Disposition: broker.DispositionDetach}})
	if resp.Type != broker.TypeError || resp.Error.Err().Code != broker.CodeNotOwner {
		t.Fatalf("unforced release by a non-owner = %s, want not_owner", resp.Type)
	}

	resp = rawCall(t, operator, &broker.Request{ID: "r2", Type: broker.TypeRelease,
		Release: &broker.ReleaseRequest{Name: "held", Disposition: broker.DispositionDetach, Force: true}})
	if resp.Type != broker.TypeReleased {
		t.Fatalf("forced release = %s %+v", resp.Type, resp.Error)
	}
	if g := grants(); g.Owned || !g.Alive || g.OwnerConn != 0 {
		t.Fatalf("after force: %+v, want unowned and alive", g)
	}
	if f.seat(1) != nil || !f.d.reg.Get("held").Alive() {
		t.Fatal("force release touched the seat process")
	}

	_ = owner.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		r, err := owner.ReadResponse()
		if err != nil {
			t.Fatalf("old owner never told: %v", err)
		}
		if r.Type == broker.TypeAgentDetached {
			if !strings.Contains(r.AgentDetached.Reason, "operator") {
				t.Fatalf("reason = %q", r.AgentDetached.Reason)
			}
			break
		}
	}

	// The host re-grants: a fresh connection takes the grant, no not_owner
	// or grant_held.
	rawSeat(t, f.sock, "held")
	if g := grants(); !g.Owned {
		t.Fatalf("re-grant did not take the grant: %+v", g)
	}
}

// dialRaw opens a bare connection that owns nothing: an operator's.
func dialRaw(t *testing.T, sock string) *broker.Conn {
	t.Helper()
	c, err := broker.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

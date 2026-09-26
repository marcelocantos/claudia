// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func launchRegistered(t *testing.T, def claudia.AgentDef) *claudia.Agent {
	t.Helper()
	reg, err := claudia.NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if def.TermLogPath == "" {
		def.TermLogPath = "-"
	}
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch(def.Name)
	if err != nil {
		t.Fatal(err)
	}
	return proc
}

func startedSeat(f *fixture, name string) *seat {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.seats {
		if s.start().Config.Name == name {
			return s
		}
	}
	return nil
}

func detachEphemeral(t *testing.T, f *fixture, proc *claudia.Agent, name string) {
	t.Helper()
	if err := proc.Detach(); err != nil {
		t.Fatal(err)
	}
	// Unowned is published in the same critical section that arms the TTL,
	// so the timer is registered once this returns.
	waitFor(t, "detached "+name, func() bool { return f.unowned(name) })
}

func (f *fixture) unowned(name string) bool {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	g := f.d.grants[name]
	return g != nil && g.owner == nil
}

// TestEphemeralOrphanTTLLeavesLongLivedGrants: a dropped plumbing seat is
// stopped after EphemeralGrantTTL. A jevons grant, and a parent-pimp seat
// whose name is not pimp-smoke-* / pimp-handoff-*, stay running.
func TestEphemeralOrphanTTLLeavesLongLivedGrants(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	smoke, err := claudia.EphemeralSeatDef(claudia.EphemeralSmoke, "ttl", claudia.PurposeWork)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := claudia.EphemeralSeatDef(claudia.EphemeralHandoff, "ttl", claudia.PurposeAside)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(smoke.WorkDir)
		_ = os.RemoveAll(handoff.WorkDir)
	})
	smokeProc := launchRegistered(t, smoke)
	handoffProc := launchRegistered(t, handoff)
	jevons := launchRegistered(t, claudia.AgentDef{
		Name: "jevons-worker", Parent: "jevons-po", Purpose: claudia.PurposeWork,
		WorkDir: t.TempDir(), SessionID: "sid-jevons",
	})
	bare := launchRegistered(t, claudia.AgentDef{
		Name: "pimp-smoke", Parent: claudia.ParentPimp, Purpose: claudia.PurposeWork,
		WorkDir: t.TempDir(), SessionID: "sid-bare",
	})

	detachEphemeral(t, f, smokeProc, smoke.Name)
	detachEphemeral(t, f, handoffProc, handoff.Name)
	if err := jevons.Detach(); err != nil {
		t.Fatal(err)
	}
	if err := bare.Detach(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "jevons detached", func() bool { return f.unowned("jevons-worker") })
	waitFor(t, "bare detached", func() bool { return f.unowned("pimp-smoke") })

	f.clock.Advance(claudia.EphemeralGrantTTL)
	waitFor(t, "smoke reaped", func() bool { return f.d.reg.Def(smoke.Name) == nil })
	waitFor(t, "handoff reaped", func() bool { return f.d.reg.Def(handoff.Name) == nil })

	if f.d.reg.Def("jevons-worker") == nil || f.d.reg.Get("jevons-worker") == nil || !f.d.reg.Get("jevons-worker").Alive() {
		t.Fatal("jevons grant was reaped with the plumbing seats")
	}
	if _, _, stops := startedSeat(f, "jevons-worker").counts(); stops != 0 {
		t.Fatalf("jevons provider was stopped")
	}
	if f.d.reg.Def("pimp-smoke") == nil || !f.d.reg.Get("pimp-smoke").Alive() {
		t.Fatal("parent pimp without a pimp-smoke-* / pimp-handoff-* name was reaped")
	}
	if _, _, stops := startedSeat(f, smoke.Name).counts(); stops != 1 {
		t.Fatalf("smoke stops = %d, want 1", stops)
	}
	if _, _, stops := startedSeat(f, handoff.Name).counts(); stops != 1 {
		t.Fatalf("handoff stops = %d, want 1", stops)
	}
}

// TestEphemeralReconnectByNameBeforeTTL: detach leaves the seat running,
// a second grant of the same name reclaims it, and the TTL from the gap
// does not stop a seat that has an owner again. A later gap still reaps.
func TestEphemeralReconnectByNameBeforeTTL(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	def, err := claudia.EphemeralSeatDef(claudia.EphemeralSmoke, "reclaim", claudia.PurposeOverseer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(def.WorkDir) })
	first := launchRegistered(t, def)
	sid := first.SessionID()
	detachEphemeral(t, f, first, def.Name)

	f.clock.Advance(claudia.EphemeralGrantTTL - time.Second)
	if f.d.reg.Def(def.Name) == nil || startedSeat(f, def.Name) == nil {
		t.Fatal("seat was reaped before the TTL")
	}

	second := launchRegistered(t, def)
	t.Cleanup(second.Stop)
	if second.SessionID() != sid {
		t.Fatalf("reclaimed session = %s, want %s", second.SessionID(), sid)
	}
	if f.seatCount() != 1 {
		t.Fatalf("reclaim started %d processes, want 1", f.seatCount())
	}
	if !f.owned(def.Name) {
		t.Fatal("reclaim did not take ownership")
	}

	// The timer from the first gap fires. The seat is owned again, so it stays.
	var kept atomic.Bool
	f.d.orphanDecision = func(name string, stopped bool) {
		if name == def.Name && !stopped {
			kept.Store(true)
		}
	}
	f.clock.Advance(claudia.EphemeralGrantTTL)
	waitFor(t, "stale timer left the reclaimed seat", func() bool { return kept.Load() })
	f.d.orphanDecision = nil
	if f.d.reg.Def(def.Name) == nil || !f.owned(def.Name) {
		t.Fatal("TTL from the unowned gap stopped a reclaimed seat")
	}
	if _, _, stops := startedSeat(f, def.Name).counts(); stops != 0 {
		t.Fatal("reclaimed provider was stopped")
	}

	detachEphemeral(t, f, second, def.Name)
	f.clock.Advance(claudia.EphemeralGrantTTL)
	waitFor(t, "reaped after the second gap", func() bool { return f.d.reg.Def(def.Name) == nil })
}

// TestEphemeralDetachDispositionArmsTTL: an explicit detach (not a dropped
// connection) also starts the orphan clock.
func TestEphemeralDetachDispositionArmsTTL(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	def, err := claudia.EphemeralSeatDef(claudia.EphemeralHandoff, "detach", claudia.PurposeWork)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(def.WorkDir) })
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: def})
	if err != nil {
		t.Fatal(err)
	}
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	resp := rawCall(t, c, &broker.Request{ID: "g", Type: broker.TypeGrant, Grant: &broker.GrantRequest{Name: def.Name, Def: raw}})
	if resp.Type != broker.TypeGranted {
		t.Fatalf("grant answered %s: %+v", resp.Type, resp.Error)
	}
	before := f.clock.Pending()
	resp = rawCall(t, c, &broker.Request{ID: "r", Type: broker.TypeRelease, Release: &broker.ReleaseRequest{
		Name: def.Name, Disposition: broker.DispositionDetach,
	}})
	if resp.Type != broker.TypeReleased {
		t.Fatalf("release answered %s: %+v", resp.Type, resp.Error)
	}
	if !f.unowned(def.Name) {
		t.Fatal("detach removed the seat instead of leaving it reclaimable")
	}
	if f.clock.Pending() <= before {
		t.Fatal("detach did not arm the orphan TTL")
	}
	f.clock.Advance(claudia.EphemeralGrantTTL)
	waitFor(t, "detach TTL reaped the seat", func() bool { return f.d.reg.Def(def.Name) == nil })
}

func TestEphemeralWorkDirIsolated(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	proc, err := claudia.Start(claudia.Config{
		Name: "pimp-smoke-iso", Provider: claudia.ProviderGrok, WorkDir: "/workspace", TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)
	got := startedSeat(f, "pimp-smoke-iso")
	if got == nil {
		t.Fatal("seat was not started")
	}
	wd := got.start().Config.WorkDir
	root, err := filepath.Abs(claudia.EphemeralWorkRoot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(wd, root+string(filepath.Separator)) {
		t.Fatalf("workdir %q was not isolated under %s", wd, root)
	}
	st, err := os.Stat(wd)
	if err != nil || !st.IsDir() {
		t.Fatalf("isolated workdir %s: %v", wd, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wd) })
	stored := f.d.reg.Def("pimp-smoke-iso")
	if stored == nil || stored.Parent != claudia.ParentPimp || stored.Purpose != claudia.PurposeWork {
		t.Fatalf("stored def = %+v", stored)
	}

	explicit := t.TempDir()
	kept, err := claudia.EphemeralSeatDef(claudia.EphemeralHandoff, "keep", claudia.PurposeWork)
	if err != nil {
		t.Fatal(err)
	}
	kept.WorkDir = explicit
	proc2 := launchRegistered(t, kept)
	t.Cleanup(proc2.Stop)
	abs, err := filepath.Abs(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if got := startedSeat(f, kept.Name).start().Config.WorkDir; got != filepath.Clean(abs) {
		t.Fatalf("explicit temp workdir = %s, want %s", got, filepath.Clean(abs))
	}
}

func TestEphemeralGrantRejectsMismatchedParentAndPurpose(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	_, err := launchExpectErr(t, claudia.AgentDef{
		Name: "pimp-smoke-foreign", Parent: "jevons-po", Purpose: claudia.PurposeWork,
		WorkDir: t.TempDir(), SessionID: "sid-foreign",
	})
	if err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("foreign parent err = %v", err)
	}
	_, err = launchExpectErr(t, claudia.AgentDef{
		Name: "pimp-handoff-bad", Parent: claudia.ParentPimp, Purpose: "coding",
		WorkDir: t.TempDir(), SessionID: "sid-bad",
	})
	if err == nil || !strings.Contains(err.Error(), "purpose") {
		t.Fatalf("bad purpose err = %v", err)
	}
	if f.seatCount() != 0 {
		t.Fatal("a refused plumbing seat was started")
	}

	jevons := launchRegistered(t, claudia.AgentDef{
		Name: "jevons-po", Parent: "overseer", Purpose: claudia.PurposeOverseer,
		WorkDir: t.TempDir(), SessionID: "sid-po",
	})
	t.Cleanup(jevons.Stop)
	if f.d.reg.Def("jevons-po") == nil || f.d.reg.Def("jevons-po").Parent != "overseer" {
		t.Fatal("jevons grant did not survive a refused plumbing grant")
	}
}

func TestEphemeralResumeReapsOrphanAndKeepsJevons(t *testing.T) {
	f := newFixture(t)
	writeGrantsTable(t, f.state, []claudia.AgentDef{
		{Name: "pimp-smoke-boot", Parent: claudia.ParentPimp, Purpose: claudia.PurposeWork,
			WorkDir: t.TempDir(), SessionID: "sid-smoke", AutoStart: true, Provider: claudia.ProviderGrok},
		{Name: "pimp-handoff-boot", Parent: claudia.ParentPimp, Purpose: claudia.PurposeAside,
			WorkDir: "/workspace", SessionID: "sid-hand", AutoStart: true, Provider: claudia.ProviderGrok},
		{Name: "pimp-smoke-foreign", Parent: "jevons-po", Purpose: claudia.PurposeWork,
			WorkDir: t.TempDir(), SessionID: "sid-foreign", AutoStart: true, Provider: claudia.ProviderGrok},
		{Name: "jevons-po", Parent: "overseer", Purpose: claudia.PurposeOverseer,
			WorkDir: t.TempDir(), SessionID: "sid-po", AutoStart: true, Provider: claudia.ProviderGrok},
	})
	f.bootWith(t, f.options(nil))

	waitFor(t, "jevons and plumbing seats resumed", func() bool {
		return nudged(f, "pimp-smoke-boot") && nudged(f, "pimp-handoff-boot") && nudged(f, "jevons-po")
	})
	if startedSeat(f, "pimp-smoke-foreign") != nil || f.d.reg.Def("pimp-smoke-foreign") != nil {
		t.Fatal("a plumbing name with a jevons parent was resumed")
	}
	root, err := filepath.Abs(claudia.EphemeralWorkRoot())
	if err != nil {
		t.Fatal(err)
	}
	hand := startedSeat(f, "pimp-handoff-boot").start().Config.WorkDir
	if !strings.HasPrefix(hand, root+string(filepath.Separator)) {
		t.Fatalf("resumed handoff workdir %q was not isolated", hand)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hand) })

	f.clock.Advance(claudia.EphemeralGrantTTL)
	waitFor(t, "resumed smoke reaped", func() bool { return f.d.reg.Def("pimp-smoke-boot") == nil })
	waitFor(t, "resumed handoff reaped", func() bool { return f.d.reg.Def("pimp-handoff-boot") == nil })
	if f.d.reg.Get("jevons-po") == nil || !f.d.reg.Get("jevons-po").Alive() {
		t.Fatal("resumed jevons seat was reaped")
	}
	if _, _, stops := startedSeat(f, "jevons-po").counts(); stops != 0 {
		t.Fatal("resumed jevons provider was stopped")
	}
}

func nudged(f *fixture, name string) bool {
	s := startedSeat(f, name)
	if s == nil {
		return false
	}
	sends := s.sent()
	return len(sends) == 1 && sends[0] == "restart-nudge"
}

func launchExpectErr(t *testing.T, def claudia.AgentDef) (*claudia.Agent, error) {
	t.Helper()
	reg, err := claudia.NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	def.TermLogPath = "-"
	if err := reg.Register(def); err != nil {
		t.Fatal(err)
	}
	return reg.Launch(def.Name)
}

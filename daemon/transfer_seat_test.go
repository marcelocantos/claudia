// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// 🎯T136: a migration context-transfer seat answers one call. Whatever way
// that call ends, the daemon keeps no grant for it, and never resumes one.
// On 2026-09-28 the shared broker held 534 of them, all auto_start, and a
// restart spent ~90s refreshing an OAuth token for each before real seats
// came back.

func transferSeatName(suffix string) string { return claudia.MigrationTransferSeatPrefix + suffix }

func transferGrant(t *testing.T, name string) *broker.Request {
	t.Helper()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: name, WorkDir: t.TempDir(), SessionID: "sid-" + name, TermLogPath: "-",
		Provider: claudia.SubscriptionSeatProvider(claudia.ProviderClaude), Model: "claude-sonnet-5",
		SummaryOnly: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &broker.Request{ID: "grant", Type: broker.TypeGrant, Grant: &broker.GrantRequest{Name: name, Def: def}}
}

func assertNoGrant(t *testing.T, f *fixture, name string) {
	t.Helper()
	if def := f.d.reg.Def(name); def != nil {
		t.Fatalf("registry still holds %s: %+v", name, *def)
	}
	if _, ok := readGrantsTable(t, f.state)[name]; ok {
		t.Fatalf("grants table on disk still holds %s", name)
	}
	f.d.mu.Lock()
	_, live := f.d.grants[name]
	f.d.mu.Unlock()
	if live {
		t.Fatalf("daemon still tracks a live grant for %s", name)
	}
}

func TestT136TransferSeatStartFailureLeavesNoGrant(t *testing.T) {
	f := newFixture(t)
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		if cfg.SummaryOnly {
			return nil, errors.New("invalid_grant: refresh token revoked")
		}
		return f.startSeat(ctx, cfg)
	}
	f.boot(t, nil)
	name := transferSeatName("start-fails")

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	resp := rawCall(t, c, transferGrant(t, name))
	if resp.Type != broker.TypeError || resp.Error == nil {
		t.Fatalf("grant of a failing transfer seat answered %s, want an error", resp.Type)
	}
	assertNoGrant(t, f, name)
}

func TestT136TransferSeatStopLeavesNoGrant(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	name := transferSeatName("released")

	// The consumer's own path: SummarizeForMigration's StartContext and its
	// deferred Stop, through the daemon.
	a, err := claudia.StartContext(context.Background(), claudia.Config{
		Name: name, WorkDir: t.TempDir(), TermLogPath: "-",
		Provider: claudia.SubscriptionSeatProvider(claudia.ProviderClaude), Model: "claude-sonnet-5",
		SummaryOnly: true,
	})
	if err != nil {
		t.Fatalf("StartContext via daemon: %v", err)
	}
	if !a.DaemonHeld() {
		t.Fatal("transfer seat is not daemon-held")
	}
	def := f.d.reg.Def(name)
	if def == nil || !def.SummaryOnly || def.AutoStart {
		t.Fatalf("held transfer seat def = %+v, want summary_only and not auto_start", def)
	}
	if def := readGrantsTable(t, f.state)[name]; def.AutoStart {
		t.Fatalf("persisted transfer seat is auto_start: %+v", def)
	}
	a.Stop()
	waitFor(t, "transfer seat released", func() bool { return f.d.reg.Def(name) == nil })
	assertNoGrant(t, f, name)
	if _, _, stops := f.seat(0).counts(); stops == 0 {
		t.Fatal("daemon-side transfer process was never stopped")
	}
}

func TestT136TransferSeatOwnerGoneLeavesNoGrant(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	name := transferSeatName("owner-gone")

	// A work seat on its own connection is the contrast: it outlives its
	// owner, unowned, as every granted seat always has.
	work := rawSeat(t, f.sock, "work-seat")

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	if resp := rawCall(t, c, transferGrant(t, name)); resp.Type != broker.TypeGranted {
		t.Fatalf("transfer grant answered %s: %+v", resp.Type, resp.Error)
	}
	// The consumer went away without releasing: a crash, a timeout that
	// closed its client, or a release that never reached the daemon.
	_ = c.Close()
	_ = work.Close()
	waitFor(t, "orphaned transfer seat removed", func() bool { return f.d.reg.Def(name) == nil })
	assertNoGrant(t, f, name)
	waitFor(t, "work seat detached", func() bool { return !f.owned("work-seat") })
	if f.d.reg.Def("work-seat") == nil {
		t.Fatal("closing a work seat's owner removed the work seat")
	}
	transferStopped := false
	for i := 0; i < f.seatCount(); i++ {
		if s := f.seat(i); s.start().Config.Name == name {
			_, _, stops := s.counts()
			transferStopped = stops > 0
		}
	}
	if !transferStopped {
		t.Fatal("orphaned transfer process was left running")
	}
}

func TestT136BootPrunesStrandedTransferSeats(t *testing.T) {
	f := newFixture(t)
	// The table as the 2026-09-28 broker left it: a transfer seat written by
	// a daemon that dropped summary_only (auto_start, name only), one that
	// kept it, and the real seats that must survive untouched.
	legacy := claudia.AgentDef{Name: transferSeatName("legacy"), SessionID: "sid-legacy", WorkDir: "/tmp",
		Provider: claudia.SubscriptionSeatProvider(claudia.ProviderClaude), Model: "claude-sonnet-5", AutoStart: true}
	current := claudia.AgentDef{Name: transferSeatName("current"), SessionID: "sid-current", WorkDir: "/tmp",
		Provider: claudia.SubscriptionSeatProvider(claudia.ProviderClaude), Model: "claude-sonnet-5", SummaryOnly: true}
	held := claudia.AgentDef{Name: "held-seat", SessionID: "sid-held", WorkDir: "/tmp", TermLogPath: "-", AutoStart: true}
	idle := claudia.AgentDef{Name: "idle-seat", SessionID: "sid-idle", WorkDir: "/tmp", TermLogPath: "-"}
	writeGrantsTable(t, f.state, []claudia.AgentDef{legacy, current, held, idle})

	opts := f.options(nil)
	f.bootWith(t, opts) // resume enabled: only held-seat may be brought back
	select {
	case <-f.d.resumeDone:
	case <-t.Context().Done():
		t.Fatal("boot resume never finished")
	}

	table := readGrantsTable(t, f.state)
	for _, name := range []string{legacy.Name, current.Name} {
		if _, ok := table[name]; ok {
			t.Fatalf("boot kept stranded transfer seat %s", name)
		}
	}
	if got, ok := table["held-seat"]; !ok || !got.AutoStart {
		t.Fatalf("boot pruned or rewrote a real auto-start seat: %+v (present=%v)", got, ok)
	}
	if _, ok := table["idle-seat"]; !ok {
		t.Fatal("boot pruned a real idle seat")
	}
	for i := 0; i < f.seatCount(); i++ {
		if name := f.seat(i).start().Config.Name; name != "held-seat" {
			t.Fatalf("boot launched %s; only held-seat should resume", name)
		}
	}
}

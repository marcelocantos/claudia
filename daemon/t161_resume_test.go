// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)

// 🎯T161: after a broker restart the fleet took 5-7 minutes to come back.
// Resume went two at a time in name order through every AutoStart grant —
// 25 on 2026-09-30, most of them workers no consumer had held in days — so
// the seats a consumer was waiting on came back last.

// writeClaims writes the claim ledger a previous daemon would have left.
func writeClaims(t *testing.T, state string, seats map[string]seatClaim) {
	t.Helper()
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(claimsDoc{Version: claimsVersion, Seats: seats})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, claimsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readClaims(t *testing.T, state string) map[string]seatClaim {
	t.Helper()
	c, err := loadSeatClaims(filepath.Join(state, claimsFile))
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]seatClaim{}
	for name, claim := range c.seats {
		out[name] = claim
	}
	return out
}

// recordStarts makes the fixture's seats record the order they start in.
func recordStarts(f *fixture) func() []string {
	var mu sync.Mutex
	var order []string
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		mu.Lock()
		order = append(order, cfg.Name)
		mu.Unlock()
		return f.startSeat(ctx, cfg)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), order...)
	}
}

func autoStart(t *testing.T, names ...string) []claudia.AgentDef {
	var defs []claudia.AgentDef
	for _, n := range names {
		defs = append(defs, claudia.AgentDef{Name: n, WorkDir: t.TempDir(), SessionID: "sid-" + n, AutoStart: true})
	}
	return defs
}

// TestT161ResumeHeldSeatsFirst: the grants a consumer held most recently
// resume first; a grant never held resumes after every held one.
func TestT161ResumeHeldSeatsFirst(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	now := f.clock.Now()
	writeGrantsTable(t, f.state, autoStart(t, "a-old-worker", "b-never-held", "claudia-po", "jevons-po"))
	writeClaims(t, f.state, map[string]seatClaim{
		"claudia-po":   {Held: now.Add(-time.Minute)},
		"jevons-po":    {Held: now.Add(-2 * time.Minute)},
		"a-old-worker": {Held: now.Add(-3 * time.Hour)},
		"b-never-held": {Seen: now.Add(-time.Hour)},
	})
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 1
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := strings.Join(starts(), ","); got != "claudia-po,jevons-po,a-old-worker,b-never-held" {
		t.Fatalf("resume order = %s, want the most recently held first", got)
	}
}

// TestT161ResumeLeavesLongUnclaimedGrants: a grant nobody has held for
// longer than ResumeUnclaimedAfter is not resumed, and is not forgotten: a
// consumer that asks for it by name is granted it.
func TestT161ResumeLeavesLongUnclaimedGrants(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	now := f.clock.Now()
	writeGrantsTable(t, f.state, autoStart(t, "a-old-worker", "b-never-held", "claudia-po"))
	writeClaims(t, f.state, map[string]seatClaim{
		"claudia-po":   {Held: now.Add(-time.Minute)},
		"a-old-worker": {Held: now.Add(-3 * 24 * time.Hour)},
		"b-never-held": {Seen: now.Add(-2 * 24 * time.Hour)},
	})
	opts := f.options(nil)
	opts.RestartNudge = "-"
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := strings.Join(starts(), ","); got != "claudia-po" {
		t.Fatalf("resumed %s, want only claudia-po", got)
	}
	if def := f.d.reg.Def("a-old-worker"); def == nil || !def.AutoStart {
		t.Fatalf("a-old-worker = %+v, want still registered", def)
	}
	a, err := claudia.Start(claudia.Config{Name: "a-old-worker", WorkDir: t.TempDir(), SessionID: "sid-a-old-worker", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("a consumer asking for the left grant: %v", err)
	}
	t.Cleanup(func() { _ = a.Detach() })
	if got := strings.Join(starts(), ","); got != "claudia-po,a-old-worker" {
		t.Fatalf("starts = %s, want the grant request to start a-old-worker", got)
	}
	if held := readClaims(t, f.state)["a-old-worker"].Held; !held.Equal(now) {
		t.Fatalf("a-old-worker held at %v, want stamped by the grant at %v", held, now)
	}
}

// TestT161UnheldGrantIsLeftAfterTheCutoff: a grant the ledger has never
// seen held gets the cutoff from the boot that first saw it — resumed on
// that boot, left on one past the cutoff with nobody holding it between.
func TestT161UnheldGrantIsLeftAfterTheCutoff(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	writeGrantsTable(t, f.state, autoStart(t, "stale"))
	opts := f.options(nil)
	opts.RestartNudge = "-"
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := strings.Join(starts(), ","); got != "stale" {
		t.Fatalf("first boot resumed %q, want stale: the ledger has no evidence against it yet", got)
	}
	if seen := readClaims(t, f.state)["stale"].Seen; !seen.Equal(f.clock.Now()) {
		t.Fatalf("stale seen at %v, want the first boot's %v", seen, f.clock.Now())
	}
	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(DefaultResumeUnclaimedAfter + time.Hour)
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := strings.Join(starts(), ","); got != "stale" {
		t.Fatalf("starts = %s: the second boot resumed a grant nobody held for a day", got)
	}
}

// TestT161ClaimLedgerFollowsOwnership: a grant, a consumer going away and a
// consumer still holding its grant at each refresh all stamp the ledger.
func TestT161ClaimLedgerFollowsOwnership(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	t0 := f.clock.Now()
	a, err := claudia.Start(claudia.Config{Name: "kept", WorkDir: t.TempDir(), SessionID: "sid-k", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if held := readClaims(t, f.state)["kept"].Held; !held.Equal(t0) {
		t.Fatalf("grant stamped %v, want %v", held, t0)
	}
	f.clock.Advance(claimRefreshInterval)
	t1 := f.clock.Now()
	waitFor(t, "refresh stamps the held grant", func() bool { return readClaims(t, f.state)["kept"].Held.Equal(t1) })
	f.clock.Advance(time.Minute)
	t2 := f.clock.Now()
	if err := a.Detach(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the owner going away stamps the grant", func() bool { return readClaims(t, f.state)["kept"].Held.Equal(t2) })
}

// TestT161MalformedClaimLedgerIsAnError: a ledger that cannot be read stops
// the daemon rather than silently resuming in name order again.
func TestT161MalformedClaimLedgerIsAnError(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.state, claimsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := f.options(nil)
	opts.DisableResume = true
	if d, err := New(opts); err == nil {
		_ = d.Close()
		t.Fatal("New accepted a malformed claim ledger")
	}
}

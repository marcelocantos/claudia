// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// writeConsumerIntent writes the consumer's fleet/intent.json under
// consumerRoot/fleet/intent.json. agents maps name -> state ("parked",
// "reaped", or anything else for live).
func writeConsumerIntent(t *testing.T, consumerRoot string, agents map[string]string) {
	t.Helper()
	dir := filepath.Join(consumerRoot, "fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`{"agents":{`)
	first := true
	for name, state := range agents {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`"` + name + `":{"state":"` + state + `"}`)
	}
	b.WriteString(`}}`)
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 🎯T233.2: a live named/PO seat and a parked seat both appear in the
// consumer's intent file; only the parked one is held back at boot.
func TestT233BootResumeSkipsPolicyHeldAuto(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	writeConsumerIntent(t, consumerRoot, map[string]string{
		"jv-t1020-auto": "parked",
	})
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po", "jv-t1020-auto"))
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 1
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := strings.Join(starts(), ","); got != "claudia-po" {
		t.Fatalf("resume starts = %q, want only claudia-po", got)
	}
}

// 🎯T233.2: a consumer state dir is configured but its intent file is
// missing — the broker fails closed and resumes NO AutoStart seat,
// including named/PO ones, rather than guessing the held set is empty.
func TestT233BootResumeFailsClosedOnMissingIntentFile(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir() // no fleet/intent.json written
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po", "jv-t1020-auto"))
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 1
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := starts(); len(got) != 0 {
		t.Fatalf("resume starts = %v, want none (fail closed)", got)
	}
}

// 🎯T233.2: a consumer state dir is configured but its intent file is
// invalid JSON — same fail-closed outcome as missing.
func TestT233BootResumeFailsClosedOnInvalidIntentFile(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	dir := filepath.Join(consumerRoot, "fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po"))
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 1
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := starts(); len(got) != 0 {
		t.Fatalf("resume starts = %v, want none (fail closed)", got)
	}
}

// 🎯T233.2: a valid intent file naming no held seats resumes everything,
// named/PO included — a configured, readable file with nothing to say
// about a name changes nothing for it.
func TestT233BootResumeUnaffectedByValidEmptyIntentFile(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	writeConsumerIntent(t, consumerRoot, map[string]string{})
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po", "jevons-po"))
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 1
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)
	<-f.d.resumeDone
	got := strings.Join(starts(), ",")
	if !strings.Contains(got, "claudia-po") || !strings.Contains(got, "jevons-po") {
		t.Fatalf("resume starts = %q, want both claudia-po and jevons-po", got)
	}
}

// 🎯T233.2: a Grant for a name the consumer's live intent file currently
// parks is refused before launch, even though AutoStart/claims would
// otherwise let it through — e.g. a fresh Grant request for an auto the
// consumer is dynamically holding for frontier_consume.disabled.
func TestT233GrantRefusedWhileConsumerIntentParksName(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir()
	writeConsumerIntent(t, consumerRoot, map[string]string{
		"jv-t1020-auto": "parked",
	})
	opts := f.options(nil)
	opts.DisableResume = true
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "jv-t1020-auto", WorkDir: t.TempDir(), SessionID: "sid-held", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := rawCall(t, c, &broker.Request{ID: "grant", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "jv-t1020-auto", Def: def}})
	if resp.Type != broker.TypeError || resp.Error == nil || resp.Error.Code != broker.CodeConsumerPolicyHold {
		t.Fatalf("grant for policy-held name answered %s %+v, want CodeConsumerPolicyHold", resp.Type, resp.Error)
	}
	if f.seatCount() != 0 {
		t.Fatalf("seat count = %d, want 0 (no launch for a policy-held name)", f.seatCount())
	}
}

// 🎯T233.2: once the consumer rewrites the file lifting the hold, an
// identical Grant succeeds — the only way to lift a hold is the consumer
// rewriting the file; nothing on the broker side (claim refresh, reattach)
// does it.
func TestT233GrantSucceedsAfterConsumerLiftsHold(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir()
	writeConsumerIntent(t, consumerRoot, map[string]string{
		"jv-t1020-auto": "parked",
	})
	opts := f.options(nil)
	opts.DisableResume = true
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "jv-t1020-auto", WorkDir: t.TempDir(), SessionID: "sid-held", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	denied := rawCall(t, c, &broker.Request{ID: "grant1", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "jv-t1020-auto", Def: def}})
	if denied.Type != broker.TypeError || denied.Error.Code != broker.CodeConsumerPolicyHold {
		t.Fatalf("first grant answered %s %+v, want CodeConsumerPolicyHold", denied.Type, denied.Error)
	}

	// The consumer lifts the hold by rewriting the file.
	writeConsumerIntent(t, consumerRoot, map[string]string{})

	granted := rawCall(t, c, &broker.Request{ID: "grant2", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "jv-t1020-auto", Def: def}})
	if granted.Type != broker.TypeGranted {
		t.Fatalf("grant after lift answered %s %+v, want granted", granted.Type, granted.Error)
	}
	if f.seatCount() != 1 {
		t.Fatalf("seat count = %d, want 1 after the lift", f.seatCount())
	}
}

// 🎯T233.2: a Grant is refused while a configured consumer state dir's
// intent file is missing/invalid — fail-closed applies to Grant/Adopt too,
// not only boot resume, until a valid snapshot exists.
func TestT233GrantFailsClosedOnMissingIntentFile(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir() // no fleet/intent.json
	opts := f.options(nil)
	opts.DisableResume = true
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "claudia-po", WorkDir: t.TempDir(), SessionID: "sid-po", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := rawCall(t, c, &broker.Request{ID: "grant", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "claudia-po", Def: def}})
	if resp.Type != broker.TypeError || resp.Error == nil || resp.Error.Code != broker.CodeConsumerPolicyHold {
		t.Fatalf("grant with missing intent file answered %s %+v, want CodeConsumerPolicyHold", resp.Type, resp.Error)
	}
}

// 🎯T233.2: a claim refresh (noteHeld) does not un-park a name the
// consumer's live intent still parks — the next Grant re-checks the file
// fresh, so cached claims-ledger state cannot revive it.
func TestT233ClaimRefreshCannotReviveHeldName(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir()
	writeConsumerIntent(t, consumerRoot, map[string]string{
		"jv-t1020-auto": "parked",
	})
	opts := f.options(nil)
	opts.DisableResume = true
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)

	// Simulate a claim refresh (or a stale held-before-restart entry)
	// having marked the name held/not-stood-down in the claims ledger —
	// the policy hold must still bind because it is read live from the
	// consumer's own file, never from this ledger.
	f.d.noteHeld("jv-t1020-auto")

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "jv-t1020-auto", WorkDir: t.TempDir(), SessionID: "sid-held", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := rawCall(t, c, &broker.Request{ID: "grant", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "jv-t1020-auto", Def: def}})
	if resp.Type != broker.TypeError || resp.Error == nil || resp.Error.Code != broker.CodeConsumerPolicyHold {
		t.Fatalf("grant after claim refresh answered %s %+v, want still held", resp.Type, resp.Error)
	}
}

// 🎯T233.2 PO follow-up: the consumer publishes a hold for an AutoStart
// auto WHILE resumeSeats is held on opts.resumeGate (a valid, empty
// snapshot was true at boot). The live re-read inside Select must still
// see the hold once the gate opens — proving resumeSeats does not decide
// from whatever was true at daemon construction.
func TestT233DelayedPolicyHoldDuringResumeGateBlocksLaunch(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	// Valid, empty snapshot at boot: nothing held yet.
	writeConsumerIntent(t, consumerRoot, map[string]string{})
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po", "jv-t1020-auto"))
	gate := make(chan struct{})
	opts := f.options(nil)
	opts.resumeGate = gate
	opts.ConsumerStateDir = consumerRoot

	f.bootWith(t, opts)

	// The consumer atomically publishes a hold for the auto seat while
	// resumeSeats is still parked on the gate.
	writeConsumerIntent(t, consumerRoot, map[string]string{
		"jv-t1020-auto": "parked",
	})
	close(gate)
	<-f.d.resumeDone

	got := strings.Join(starts(), ",")
	if got != "claudia-po" {
		t.Fatalf("resume starts = %q, want only claudia-po (jv-t1020-auto held during the gate wait)", got)
	}
}

// 🎯T233.2 PO follow-up: the snapshot is MISSING at daemon construction
// (so mergeConsumerIntentStandDown's early read sees nothing to merge)
// but the writer publishes a valid file before the gate opens. The live
// re-read must use that valid file, not fail closed on stale "missing"
// information from construction time.
func TestT233SnapshotBecomesValidDuringResumeGateWait(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir() // no fleet/intent.json at boot
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po", "jv-t1020-auto"))
	gate := make(chan struct{})
	opts := f.options(nil)
	opts.resumeGate = gate
	opts.ConsumerStateDir = consumerRoot

	f.bootWith(t, opts)

	// The writer publishes a valid snapshot — with a hold on the auto —
	// before the gate opens.
	writeConsumerIntent(t, consumerRoot, map[string]string{
		"jv-t1020-auto": "parked",
	})
	close(gate)
	<-f.d.resumeDone

	got := strings.Join(starts(), ",")
	if got != "claudia-po" {
		t.Fatalf("resume starts = %q, want only claudia-po (valid snapshot published before the gate opened)", got)
	}
}

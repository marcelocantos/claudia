// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// 🎯T233.2 final owner schema: ONE consumer state root holds TWO files —
// fleet/intent.json (genuine parked/reaped agent intent ONLY) and
// fleet/policy-hold.json (transient policy-held auto seats ONLY,
// frontier_consume_disabled | target_achieved). The broker enforces the
// UNION of both. Helpers below default-fill whichever file a given test
// does not care about with a valid, empty snapshot, so a test naming only
// one file's content is not accidentally exercising fail-closed on the
// other.

// writeConsumerIntent writes the consumer's fleet/intent.json. agents maps
// name -> state ("parked", "reaped", or anything else for live). If
// fleet/policy-hold.json does not already exist under consumerRoot, a
// valid empty one is written too, so a caller testing intent.json alone
// is not inadvertently failing closed on a missing policy-hold file.
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
	policyHoldPath := filepath.Join(dir, "policy-hold.json")
	if _, err := os.Stat(policyHoldPath); os.IsNotExist(err) {
		writePolicyHoldRaw(t, policyHoldPath, map[string]string{})
	}
}

// writePolicyHold writes the consumer's fleet/policy-hold.json. holds maps
// name -> reason (e.g. claudia.PolicyHoldReasonFrontierConsumeDisabled).
// If fleet/intent.json does not already exist under consumerRoot, a valid
// empty one is written too, mirroring writeConsumerIntent's default-fill.
func writePolicyHold(t *testing.T, consumerRoot string, holds map[string]string) {
	t.Helper()
	dir := filepath.Join(consumerRoot, "fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writePolicyHoldRaw(t, filepath.Join(dir, "policy-hold.json"), holds)
	intentPath := filepath.Join(dir, "intent.json")
	if _, err := os.Stat(intentPath); os.IsNotExist(err) {
		if err := os.WriteFile(intentPath, []byte(`{"agents":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writePolicyHoldRaw(t *testing.T, path string, holds map[string]string) {
	t.Helper()
	var entries []string
	for name, reason := range holds {
		entries = append(entries, fmt.Sprintf(`{"name":%q,"reason":%q}`, name, reason))
	}
	body := fmt.Sprintf(`{"version":1,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[%s]}`, strings.Join(entries, ","))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 🎯T233.2: a live named/PO seat and a transiently policy-held auto both
// exist; only the held one is held back at boot. The hold is recorded in
// fleet/policy-hold.json, not fleet/intent.json (final owner schema: the
// two files have distinct, non-overlapping meanings).
func TestT233BootResumeSkipsPolicyHeldAuto(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	writePolicyHold(t, consumerRoot, map[string]string{
		"jv-t1020-auto": claudia.PolicyHoldReasonFrontierConsumeDisabled,
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

// 🎯T233.2: a consumer state dir is configured but BOTH files are
// missing — the broker fails closed and resumes NO AutoStart seat,
// including named/PO ones, rather than guessing the held set is empty.
func TestT233BootResumeFailsClosedOnMissingIntentFile(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir() // no fleet/intent.json or fleet/policy-hold.json
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

// 🎯T233.2: fleet/intent.json is invalid JSON (fleet/policy-hold.json is
// valid and empty) — same fail-closed outcome: a writer that gets one
// file right and the other wrong is not "half safe".
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
	writePolicyHoldRaw(t, filepath.Join(dir, "policy-hold.json"), map[string]string{})
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

// 🎯T233.2: fleet/policy-hold.json is missing (fleet/intent.json is valid
// and empty) — fail-closed applies symmetrically to either file, not only
// fleet/intent.json.
func TestT233BootResumeFailsClosedOnMissingPolicyHoldFile(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	dir := filepath.Join(consumerRoot, "fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(`{"agents":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// fleet/policy-hold.json deliberately absent.
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po"))
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 1
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := starts(); len(got) != 0 {
		t.Fatalf("resume starts = %v, want none (fail closed on missing policy-hold file)", got)
	}
}

// 🎯T233.2: fleet/policy-hold.json is invalid (wrong version) — fail
// closed, same as an invalid fleet/intent.json.
func TestT233BootResumeFailsClosedOnInvalidPolicyHoldFile(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	dir := filepath.Join(consumerRoot, "fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(`{"agents":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy-hold.json"), []byte(`{"version":2,"kind":"policy_hold","updated_at":"2026-10-28T12:00:00Z","holds":[]}`), 0o600); err != nil {
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
		t.Fatalf("resume starts = %v, want none (fail closed on invalid policy-hold file)", got)
	}
}

// 🎯T233.2: both files valid and empty resumes everything, named/PO
// included — a configured, readable pair of files with nothing to say
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

// 🎯T233.2: a Grant for a name fleet/intent.json currently parks (genuine
// parked/reaped) is refused before launch.
func TestT233GrantRefusedWhileConsumerIntentParksName(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir()
	writeConsumerIntent(t, consumerRoot, map[string]string{
		"some-worker": "parked",
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
		Name: "some-worker", WorkDir: t.TempDir(), SessionID: "sid-held", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := rawCall(t, c, &broker.Request{ID: "grant", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "some-worker", Def: def}})
	if resp.Type != broker.TypeError || resp.Error == nil || resp.Error.Code != broker.CodeConsumerPolicyHold {
		t.Fatalf("grant for intent-parked name answered %s %+v, want CodeConsumerPolicyHold", resp.Type, resp.Error)
	}
	if f.seatCount() != 0 {
		t.Fatalf("seat count = %d, want 0 (no launch for a parked name)", f.seatCount())
	}
}

// 🎯T233.2: a Grant for a name fleet/policy-hold.json currently holds
// (transient policy-held auto) is refused before launch, even though
// AutoStart/claims would otherwise let it through.
func TestT233GrantRefusedWhileConsumerPolicyHoldsName(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir()
	writePolicyHold(t, consumerRoot, map[string]string{
		"jv-t1020-auto": claudia.PolicyHoldReasonFrontierConsumeDisabled,
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

// 🎯T233.2: once the consumer rewrites fleet/policy-hold.json lifting the
// hold, an identical Grant succeeds — the only way to lift a hold is the
// consumer rewriting the relevant file; nothing on the broker side
// (claim refresh, reattach) does it.
func TestT233GrantSucceedsAfterConsumerLiftsHold(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir()
	writePolicyHold(t, consumerRoot, map[string]string{
		"jv-t1020-auto": claudia.PolicyHoldReasonFrontierConsumeDisabled,
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

	// The consumer lifts the hold by rewriting fleet/policy-hold.json.
	writePolicyHold(t, consumerRoot, map[string]string{})

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
// files are missing — fail-closed applies to Grant/Adopt too, not only
// boot resume, until a valid snapshot exists for both files.
func TestT233GrantFailsClosedOnMissingIntentFile(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir() // neither file written
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
		t.Fatalf("grant with missing state files answered %s %+v, want CodeConsumerPolicyHold", resp.Type, resp.Error)
	}
}

// 🎯T233.2: a claim refresh (noteHeld) does not un-park a name
// fleet/policy-hold.json still holds — the next Grant re-checks both
// files fresh, so cached claims-ledger state cannot revive it.
func TestT233ClaimRefreshCannotReviveHeldName(t *testing.T) {
	f := newFixture(t)
	consumerRoot := t.TempDir()
	writePolicyHold(t, consumerRoot, map[string]string{
		"jv-t1020-auto": claudia.PolicyHoldReasonFrontierConsumeDisabled,
	})
	opts := f.options(nil)
	opts.DisableResume = true
	opts.ConsumerStateDir = consumerRoot
	f.bootWith(t, opts)

	// Simulate a claim refresh (or a stale held-before-restart entry)
	// having marked the name held/not-stood-down in the claims ledger —
	// the policy hold must still bind because it is read live from the
	// consumer's own files, never from this ledger.
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

// 🎯T233.2: the consumer publishes a hold for an AutoStart auto in
// fleet/policy-hold.json WHILE resumeSeats is held on opts.resumeGate (a
// valid, empty pair of snapshots was true at boot). The live re-read
// inside Select must still see the hold once the gate opens — proving
// resumeSeats does not decide from whatever was true at daemon
// construction.
func TestT233DelayedPolicyHoldDuringResumeGateBlocksLaunch(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	// Valid, empty snapshots at boot: nothing held yet.
	writeConsumerIntent(t, consumerRoot, map[string]string{})
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po", "jv-t1020-auto"))
	gate := make(chan struct{})
	opts := f.options(nil)
	opts.resumeGate = gate
	opts.ConsumerStateDir = consumerRoot

	f.bootWith(t, opts)

	// The consumer atomically publishes a hold for the auto seat in
	// fleet/policy-hold.json while resumeSeats is still parked on the
	// gate.
	writePolicyHold(t, consumerRoot, map[string]string{
		"jv-t1020-auto": claudia.PolicyHoldReasonFrontierConsumeDisabled,
	})
	close(gate)
	<-f.d.resumeDone

	got := strings.Join(starts(), ",")
	if got != "claudia-po" {
		t.Fatalf("resume starts = %q, want only claudia-po (jv-t1020-auto held during the gate wait)", got)
	}
}

// 🎯T233.2: the policy-hold snapshot is MISSING at daemon construction
// (so the early merge sees nothing to merge, and fails closed) but the
// writer publishes a valid file with a hold before the gate opens. The
// live re-read must use that valid file, not fail closed on stale
// "missing" information from construction time.
func TestT233SnapshotBecomesValidDuringResumeGateWait(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	consumerRoot := t.TempDir()
	dir := filepath.Join(consumerRoot, "fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(`{"agents":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// fleet/policy-hold.json deliberately absent at boot.
	writeGrantsTable(t, f.state, autoStart(t, "claudia-po", "jv-t1020-auto"))
	gate := make(chan struct{})
	opts := f.options(nil)
	opts.resumeGate = gate
	opts.ConsumerStateDir = consumerRoot

	f.bootWith(t, opts)

	// The writer publishes a valid policy-hold snapshot — with a hold on
	// the auto — before the gate opens.
	writePolicyHold(t, consumerRoot, map[string]string{
		"jv-t1020-auto": claudia.PolicyHoldReasonFrontierConsumeDisabled,
	})
	close(gate)
	<-f.d.resumeDone

	got := strings.Join(starts(), ",")
	if got != "claudia-po" {
		t.Fatalf("resume starts = %q, want only claudia-po (valid snapshot published before the gate opened)", got)
	}
}

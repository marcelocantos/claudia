// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// errNoCodex is the resolver's own wording, as the seat would have hit it.
var errNoCodex = errors.New("codex executable not found in PATH or known install dirs (set CODEX_BIN to override)")

// noCodex resolves every fleet CLI but codex, on both surfaces, and
// records which surfaces were asked about.
func noCodex(sessions *atomic.Int32, tasks *atomic.Int32) func(claudia.Provider, bool) error {
	return func(p claudia.Provider, session bool) error {
		if session {
			sessions.Add(1)
		} else {
			tasks.Add(1)
		}
		if p == claudia.ProviderCodex {
			return errNoCodex
		}
		return nil
	}
}

func TestGrantPickByRemainingSkipsProviderWithoutBinary(t *testing.T) {
	f := newFixture(t)
	var sessions, tasks atomic.Int32
	opts := f.options(fleetSnap(t, 15, 40, 30, 97))
	opts.DisableResume = true
	opts.resolveBinary = noCodex(&sessions, &tasks)
	f.bootWith(t, opts)
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })

	name := "pick-no-codex"
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: name, WorkDir: t.TempDir(), SessionID: "sid-no-codex", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	resp := rawCall(t, c, &broker.Request{ID: "g1", Type: broker.TypeGrant, Grant: &broker.GrantRequest{
		Name: name, Def: raw, Pick: claudia.PickRemaining,
	}})
	if resp.Type != broker.TypeGranted {
		t.Fatalf("grant answered %s: %+v", resp.Type, resp.Error)
	}
	if resp.Granted.Provider != broker.Provider(claudia.ProviderGrok) {
		t.Fatalf("provider = %s, want grok", resp.Granted.Provider)
	}
	if sessions.Load() == 0 {
		t.Fatal("a grant pick must check the Session surface")
	}
}

// `claudia broker usage` runs in the caller's process, with the caller's
// PATH; the daemon is what spawns. So the daemon reports what it cannot
// launch, and the roster marks it admit=false, reason binary_not_found.
func TestUsageReportsUnlaunchableProviders(t *testing.T) {
	f := newFixture(t)
	var sessions, tasks atomic.Int32
	opts := f.options(fleetSnap(t, 15, 40, 30, 97))
	opts.DisableResume = true
	opts.resolveBinary = noCodex(&sessions, &tasks)
	f.bootWith(t, opts)
	waitFor(t, "usage fetched", func() bool { return f.fetchCount() >= 1 })

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	resp := rawCall(t, c, &broker.Request{ID: "u1", Type: broker.TypeUsage, Usage: &broker.UsageRequest{}})
	if resp.Usage == nil {
		t.Fatalf("usage answered %s: %+v", resp.Type, resp.Error)
	}
	if got := resp.Usage.Unlaunchable; len(got) != 1 || got["codex"] != errNoCodex.Error() {
		t.Fatalf("unlaunchable = %v, want only codex with the resolver error", got)
	}
	snap := claudia.ExcludeUnlaunchable(claudia.ProjectFleetUsage(fleetSnap(t, 15, 40, 30, 97), f.clock.Now(), f.clock.Now(), nil),
		map[claudia.Provider]string{claudia.ProviderCodex: resp.Usage.Unlaunchable["codex"]})
	for _, row := range snap.Providers {
		if row.Provider == claudia.ProviderCodex && (row.Admit || !strings.HasPrefix(row.Reason, claudia.FleetReasonBinaryNotFound)) {
			t.Fatalf("codex row = %+v, want admit=false reason=binary_not_found", row)
		}
		if row.Provider != claudia.ProviderCodex && !row.Admit {
			t.Fatalf("%s row not admitted: %+v", row.Provider, row)
		}
	}
}

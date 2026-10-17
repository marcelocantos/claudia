// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSeatTranscript puts fixtureLines at the transcript path a seat reads.
func writeSeatTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(fixtureLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryRewindRelaunchesOnTheRewoundTranscript (🎯T75.8): with no
// daemon, Registry.Rewind refuses before it stops the seat or truncates
// the transcript. The relaunch that used to follow the truncation is gone.
func TestRegistryRewindRelaunchesOnTheRewoundTranscript(t *testing.T) {
	f := newSeatFixture(t)
	f.open(t, []AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid-rw"}})
	old, err := f.reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	oldBackend := f.backends["seat"]
	f.mu.Unlock()
	writeSeatTranscript(t, old.JSONLPath())
	before := mustRead(t, old.JSONLPath())

	next, res, err := f.reg.Rewind(context.Background(), "seat", 1)
	if next != nil || res != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Rewind = (%v, %+v, %v), want legacy refusal", next, res, err)
	}
	if f.reg.Get("seat") != old {
		t.Fatal("rewind replaced the seat")
	}
	oldBackend.mu.Lock()
	stops := oldBackend.stops
	oldBackend.mu.Unlock()
	if stops != 0 {
		t.Fatalf("old process stops = %d, want 0", stops)
	}
	got := mustRead(t, old.JSONLPath())
	if !bytes.Equal(got, before) || !bytes.Contains(got, []byte("CHARLIE")) {
		t.Fatalf("transcript changed:\n%s", got)
	}
}

// TestRegistryRewindHoldsTheSeatAcrossTruncation: Rewind returns before
// the truncation hook, so a Launch parked inside that hook never runs and
// the transcript is not the thing the seat is waiting on.
func TestRegistryRewindHoldsTheSeatAcrossTruncation(t *testing.T) {
	f := newSeatFixture(t)
	f.open(t, []AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid-race"}})
	first, err := f.reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	writeSeatTranscript(t, first.JSONLPath())
	before := mustRead(t, first.JSONLPath())

	called := false
	prev := registryRewindJSONL
	t.Cleanup(func() { registryRewindJSONL = prev })
	registryRewindJSONL = func(p string, n int) (*RewindResult, error) {
		called = true
		return prev(p, n)
	}

	next, res, err := f.reg.Rewind(context.Background(), "seat", 1)
	if next != nil || res != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Rewind = (%v, %+v, %v), want legacy refusal", next, res, err)
	}
	if called {
		t.Fatal("rewind truncated the transcript")
	}
	if f.reg.Get("seat") != first {
		t.Fatal("rewind replaced the seat")
	}
	if got := mustRead(t, first.JSONLPath()); !bytes.Equal(got, before) || !bytes.Contains(got, []byte("CHARLIE")) {
		t.Fatalf("transcript changed:\n%s", got)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
// daemon, Registry.Rewind stops the seat, truncates by whole user turns,
// relaunches on the same session, and keeps the relaunched handle.
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

	next, res, err := f.reg.Rewind(context.Background(), "seat", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsRemoved != 1 || res.SessionID != "sid-rw" || res.BackupPath == "" {
		t.Fatalf("result = %+v", res)
	}
	if next == old || f.reg.Get("seat") != next || next.SessionID() != "sid-rw" {
		t.Fatalf("registry holds %p (old %p, returned %p) on session %q", f.reg.Get("seat"), old, next, next.SessionID())
	}
	oldBackend.mu.Lock()
	stops := oldBackend.stops
	oldBackend.mu.Unlock()
	if stops != 1 {
		t.Fatalf("old process stops = %d, want 1", stops)
	}
	got := mustRead(t, old.JSONLPath())
	if bytes.Contains(got, []byte("CHARLIE")) || !bytes.Contains(got, []byte("turn2")) {
		t.Fatalf("transcript not rewound by one turn:\n%s", got)
	}
	if req := f.backends["seat"].request(t); !req.Resuming && !req.Config.RequireResume {
		t.Fatalf("relaunch did not resume the session: %+v", req)
	}
}

// TestRegistryRewindHoldsTheSeatAcrossTruncation: a Launch racing a Rewind
// in the gap between stopping the seat and truncating its transcript cannot
// start the seat on the old conversation; it waits, and gets the relaunched
// handle.
func TestRegistryRewindHoldsTheSeatAcrossTruncation(t *testing.T) {
	f := newSeatFixture(t)
	var path string
	var mu sync.Mutex
	var untruncatedStarts int
	launch := registryStart
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte("CHARLIE")) {
			mu.Lock()
			untruncatedStarts++
			mu.Unlock()
		}
		return launch(ctx, cfg)
	}
	f.open(t, []AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid-race"}})
	first, err := f.reg.Launch("seat")
	if err != nil {
		t.Fatal(err)
	}
	path = first.JSONLPath()
	writeSeatTranscript(t, path)
	mu.Lock()
	untruncatedStarts = 0 // the first launch predates the transcript
	mu.Unlock()

	racer := make(chan *Agent, 1)
	prev := registryRewindJSONL
	t.Cleanup(func() { registryRewindJSONL = prev })
	registryRewindJSONL = func(p string, n int) (*RewindResult, error) {
		go func() {
			a, _ := f.reg.Launch("seat")
			racer <- a
		}()
		time.Sleep(50 * time.Millisecond)
		return prev(p, n)
	}

	next, _, err := f.reg.Rewind(context.Background(), "seat", 1)
	if err != nil {
		t.Fatal(err)
	}
	raced := <-racer
	mu.Lock()
	defer mu.Unlock()
	if untruncatedStarts != 0 {
		t.Fatalf("%d start(s) ran on the transcript before truncation", untruncatedStarts)
	}
	if raced != next || f.reg.Get("seat") != next {
		t.Fatal("the racing Launch did not get the rewound seat")
	}
}

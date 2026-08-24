// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestIsOrphanCursorACP(t *testing.T) {
	acp := "/Users/x/.local/bin/cursor-agent --use-system-ca /v/index.js --force --trust --approve-mcps acp"
	resume := "/Users/x/.local/bin/agent --use-system-ca /v/index.js --force resume"
	cases := []struct {
		ppid int
		cmd  string
		want bool
	}{
		{1, acp, true},
		{27024, acp, false},
		{1, resume, false},
		{1, "cursor-agent --approve-mcps", false},
		{1, "", false},
	}
	for _, tc := range cases {
		if got := IsOrphanCursorACP(tc.ppid, tc.cmd); got != tc.want {
			t.Fatalf("ppid=%d cmd=%q: got %v want %v", tc.ppid, tc.cmd, got, tc.want)
		}
	}
}

func TestParsePSLine(t *testing.T) {
	pid, ppid, cmd, ok := parsePSLine(" 15990     1 /Users/x/.local/bin/cursor-agent --force acp")
	if !ok || pid != 15990 || ppid != 1 {
		t.Fatalf("pid=%d ppid=%d ok=%v", pid, ppid, ok)
	}
	if !strings.Contains(cmd, "cursor-agent") || !strings.Contains(cmd, " acp") {
		t.Fatalf("command = %q", cmd)
	}
}

func TestShouldKillStoreWriter(t *testing.T) {
	if shouldKillStoreWriter(1, 0, 27024) || shouldKillStoreWriter(27024, 0, 27024) {
		t.Fatal("must not signal pid 1 or the coordinator")
	}
	if !shouldKillStoreWriter(15990, 0, 27024) {
		t.Fatal("orphan writer must be reaped")
	}
	if !shouldKillStoreWriter(77415, 0, 27024) {
		t.Fatal("this coordinator's leftover child must be reaped (failed load that dropped stdout)")
	}
}

func TestReapCursorACPLeftoversKillsWritersAndExtra(t *testing.T) {
	var killed []int
	oldList, oldKill := listStoreWriters, killPIDFn
	t.Cleanup(func() {
		listStoreWriters = oldList
		killPIDFn = oldKill
	})
	listStoreWriters = func(path string) []int {
		if path == "" {
			t.Fatal("empty store path")
		}
		return []int{27606, 39004}
	}
	killPIDFn = func(pid int) { killed = append(killed, pid) }

	got := ReapCursorACPLeftovers("0beb2254-054a-45c0-8c2f-afc545afa986", 15990)
	want := []int{15990, 27606, 39004}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reaped %v want %v", got, want)
	}
	if !reflect.DeepEqual(killed, want) {
		t.Fatalf("killed %v want %v", killed, want)
	}
}

func TestReapCursorACPLeftoversDedups(t *testing.T) {
	var killed []int
	oldList, oldKill := listStoreWriters, killPIDFn
	t.Cleanup(func() {
		listStoreWriters = oldList
		killPIDFn = oldKill
	})
	listStoreWriters = func(string) []int { return []int{15990, 15990} }
	killPIDFn = func(pid int) { killed = append(killed, pid) }

	got := ReapCursorACPLeftovers("sid", 15990)
	if !slices.Equal(got, []int{15990}) {
		t.Fatalf("got %v", got)
	}
}

func TestReapOrphanCursorACP(t *testing.T) {
	var killed []int
	oldList, oldKill := listOrphanCursorACP, killPIDFn
	t.Cleanup(func() {
		listOrphanCursorACP = oldList
		killPIDFn = oldKill
	})
	listOrphanCursorACP = func() []int { return []int{14445, 15990, 1} }
	killPIDFn = func(pid int) { killed = append(killed, pid) }

	got := ReapOrphanCursorACP()
	if !slices.Equal(got, []int{14445, 15990}) {
		t.Fatalf("got %v", got)
	}
}

func TestRegistryAdoptCursorPIDDoesNotStart(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name:       "jevons",
		WorkDir:    t.TempDir(),
		SessionID:  "sid-cursor",
		Provider:   ProviderCursor,
		ConnectPID: 15990,
		AutoStart:  true,
	}); err != nil {
		t.Fatal(err)
	}

	var killed []int
	oldList, oldKill := listStoreWriters, killPIDFn
	t.Cleanup(func() {
		listStoreWriters = oldList
		killPIDFn = oldKill
	})
	listStoreWriters = func(string) []int { return []int{27606} }
	killPIDFn = func(pid int) { killed = append(killed, pid) }

	_, err = reg.Adopt("jevons")
	if !errors.Is(err, ErrNoSessionWindow) {
		t.Fatalf("Adopt: %v, want ErrNoSessionWindow (must not Start/spawn)", err)
	}
	if !slices.Contains(killed, 15990) || !slices.Contains(killed, 27606) {
		t.Fatalf("adopt must reap leftover writers, killed=%v", killed)
	}
}

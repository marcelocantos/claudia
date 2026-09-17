// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker/brokertest"
)

// startPoolHost prepares real tmux on a private server and a fake claude
// that draws the real prompt box and sits at it, so the library pool runs
// for real without a provider. It returns a function listing the pool
// windows on that server.
func startPoolHost(t *testing.T) func() []string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	fake := brokertest.Build(t)
	dir, err := os.MkdirTemp("/tmp", "cpl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	scenario := filepath.Join(dir, "scenario.json")
	raw, _ := json.Marshal(brokertest.Scenario{Ready: brokertest.ReadyPromptBox, Linger: true})
	if err := os.WriteFile(scenario, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The tmux server's environment is scrubbed, so the scenario rides in
	// a wrapper rather than in this process's environment.
	wrapper := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nFAKE_CLAUDE_SCENARIO='" + scenario + "' exec '" + fake.Bin + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", wrapper)
	sock := filepath.Join(dir, "tmux.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	return func() []string {
		out, err := exec.Command("tmux", "-S", sock, "list-windows", "-a", "-F", "#{window_id} #{window_name}").Output()
		if err != nil {
			return nil
		}
		var ids []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if id, name, ok := strings.Cut(line, " "); ok && strings.HasPrefix(name, "claudia-pool-") {
				ids = append(ids, id)
			}
		}
		return ids
	}
}

// TestAcquireSharesOneWarmSeatAcrossConsumers is 🎯T64: with a daemon
// listening, Acquire is the daemon's pool. One consumer acquires and
// returns a seat; a second consumer's Acquire gets the same warm window
// without a second spawn; drop kills it.
func TestAcquireSharesOneWarmSeatAcrossConsumers(t *testing.T) {
	poolWindows := startPoolHost(t)
	f := newFixture(t)
	f.boot(t, nil)
	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := claudia.Acquire(ctx, claudia.Config{WorkDir: workDir, TermLogPath: "-"})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if !first.DaemonHeld() {
		t.Fatal("Acquire with a daemon listening did not go through the daemon")
	}
	window := first.WindowID()
	if got := poolWindows(); len(got) != 1 || got[0] != window {
		t.Fatalf("pool windows after first acquire = %v, want [%s]", got, window)
	}
	if err := first.Release("return"); err != nil {
		t.Fatalf("Release(return): %v", err)
	}
	waitFor(t, "seat back in the pool", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		return len(f.d.grants) == 0
	})

	second, err := claudia.Acquire(ctx, claudia.Config{WorkDir: workDir, TermLogPath: "-"})
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if second.WindowID() != window {
		t.Fatalf("second consumer got window %s, want the returned warm window %s", second.WindowID(), window)
	}
	if got := poolWindows(); len(got) != 1 {
		t.Fatalf("pool windows after warm acquire = %v, want the one warm window", got)
	}
	if err := second.Release("drop"); err != nil {
		t.Fatalf("Release(drop): %v", err)
	}
	waitFor(t, "dropped window gone", func() bool { return len(poolWindows()) == 0 })
}

// TestAcquiredSeatReturnsWhenConsumerLeaves (🎯T64): a consumer that goes
// away without releasing does not leave its seat held; the next Acquire
// gets it warm.
func TestAcquiredSeatReturnsWhenConsumerLeaves(t *testing.T) {
	poolWindows := startPoolHost(t)
	f := newFixture(t)
	f.boot(t, nil)
	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := claudia.Acquire(ctx, claudia.Config{WorkDir: workDir, TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	window := first.WindowID()
	if err := first.Detach(); err != nil { // the consumer goes away
		t.Fatal(err)
	}
	waitFor(t, "seat returned", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		return len(f.d.grants) == 0
	})
	second, err := claudia.Acquire(ctx, claudia.Config{WorkDir: workDir, TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Release("drop") })
	if second.WindowID() != window || len(poolWindows()) != 1 {
		t.Fatalf("after the consumer left, Acquire got %s (windows %v), want warm %s", second.WindowID(), poolWindows(), window)
	}
}

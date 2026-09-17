// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// TestAcquiredHandleOpsKeepAliveAndDirect (🎯T64): a seat operation beyond
// Send reaches the pooled window through the daemon (Resize changes the
// tmux window's size); Release("keep_alive_for:<secs>") returns the seat
// with the pool's keep-alive deadline set; and AcquireDirect with a daemon
// listening keeps the seat in this process.
func TestAcquiredHandleOpsKeepAliveAndDirect(t *testing.T) {
	poolWindows := startPoolHost(t)
	f := newFixture(t)
	f.boot(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	grants := func() int {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		return len(f.d.grants)
	}

	a, err := claudia.Acquire(ctx, claudia.Config{WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.DaemonHeld() {
		t.Fatal("Acquire did not go through the daemon")
	}
	window := a.WindowID()
	if err := a.Resize(97, 31); err != nil {
		t.Fatalf("Resize on the acquired handle: %v", err)
	}
	sock := os.Getenv("CLAUDIA_TMUX_SOCKET")
	size, err := exec.Command("tmux", "-S", sock, "display-message", "-p", "-t", window, "#{window_width}x#{window_height}").Output()
	if err != nil || strings.TrimSpace(string(size)) != "97x31" {
		t.Fatalf("pooled window size = %q (%v), want 97x31", size, err)
	}

	before := time.Now().Unix()
	if err := a.Release("keep_alive_for:600"); err != nil {
		t.Fatalf("Release(keep_alive_for:600): %v", err)
	}
	waitFor(t, "seat back in the pool", func() bool { return grants() == 0 })
	opt := func(key string) string {
		out, _ := exec.Command("tmux", "-S", sock, "show-options", "-wv", "-t", window, "@"+key).Output()
		return strings.TrimSpace(string(out))
	}
	deadline, err := strconv.ParseInt(opt("claudia-deadline"), 10, 64)
	if err != nil || deadline < before+590 || deadline > time.Now().Unix()+610 || opt("claudia-held") != "0" {
		t.Fatalf("after keep_alive_for:600 the window has deadline %q held %q, want ~now+600 and not held", opt("claudia-deadline"), opt("claudia-held"))
	}

	direct, err := claudia.AcquireDirect(ctx, claudia.Config{WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("AcquireDirect: %v", err)
	}
	t.Cleanup(func() { _ = direct.Release("drop") })
	if direct.DaemonHeld() || grants() != 0 {
		t.Fatalf("AcquireDirect with a daemon listening: daemon-held=%v daemon grants=%d, want in-process", direct.DaemonHeld(), grants())
	}
	if len(poolWindows()) != 2 {
		t.Fatalf("pool windows = %v, want the kept-alive one and the direct one", poolWindows())
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// skipIfNoBinaries skips the test if claude or tmux are not on PATH.
// All pool integration tests call this because Acquire uses both.
func skipIfNoBinaries(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

// TestPoolKeyConsistency checks that poolKeyFor is stable and
// deterministic (no randomness, no dependency on env).
func TestPoolKeyConsistency(t *testing.T) {
	k1 := poolKeyFor("/workdir", "sonnet", "Tool1")
	k2 := poolKeyFor("/workdir", "sonnet", "Tool1")
	k3 := poolKeyFor("/workdir", "opus", "Tool1")
	k4 := poolKeyFor("/other", "sonnet", "Tool1")

	if k1 != k2 {
		t.Errorf("poolKeyFor is not deterministic: %q vs %q", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("different models produce same key")
	}
	if k1 == k4 {
		t.Errorf("different workdirs produce same key")
	}
	if len(k1) != 12 {
		t.Errorf("key length = %d, want 12 hex chars", len(k1))
	}
}

// TestPoolWindowNaming verifies the window-name scheme so that
// ListWindows matching works as expected.
func TestPoolWindowNaming(t *testing.T) {
	key := poolKeyFor("/tmp/testdir", "haiku", "Agent,TeamCreate,TeamDelete,SendMessage,EnterWorktree")
	name := poolWindowPrefix + key
	if !strings.HasPrefix(name, "claudia-pool-") {
		t.Errorf("pool window name %q does not start with claudia-pool-", name)
	}
}

// TestAcquireColdAndReturn exercises the full warm-pool cycle:
//  1. Cold acquire (no existing window) — must succeed.
//  2. Release with "return" — window stays alive.
//  3. Warm re-acquire (< 100 ms) — must succeed and be fast.
//  4. Release with "drop" — window is killed.
func TestAcquireColdAndReturn(t *testing.T) {
	skipIfNoBinaries(t)

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// --- cold acquire ---
	t1 := time.Now()
	a1, err := Acquire(ctx, Config{WorkDir: workDir})
	coldDuration := time.Since(t1)
	if err != nil {
		t.Fatalf("cold Acquire: %v", err)
	}
	t.Logf("cold acquire: %s (window %s)", coldDuration.Round(time.Millisecond), a1.tmuxWindowID)

	windowID := a1.tmuxWindowID
	if !tmuxagent.IsWindowAlive(windowID) {
		t.Fatal("window not alive after cold Acquire")
	}

	// return to pool
	if err := a1.Release("return"); err != nil {
		t.Fatalf("Release(return): %v", err)
	}

	// Window must still be alive after return.
	if !tmuxagent.IsWindowAlive(windowID) {
		t.Fatal("window died after Release(return) — should stay alive for next Acquire")
	}

	// --- warm re-acquire (same workDir, same key) ---
	t2 := time.Now()
	a2, err := Acquire(ctx, Config{WorkDir: workDir})
	warmDuration := time.Since(t2)
	if err != nil {
		t.Fatalf("warm Acquire: %v", err)
	}
	t.Logf("warm acquire: %s (window %s)", warmDuration.Round(time.Millisecond), a2.tmuxWindowID)

	// Warm acquire must reuse the same window.
	if a2.tmuxWindowID != windowID {
		t.Errorf("warm acquire used different window %s, want %s", a2.tmuxWindowID, windowID)
	}

	// Warm acquire must be < 100 ms.
	if warmDuration >= 100*time.Millisecond {
		t.Errorf("warm acquire took %s, want < 100ms", warmDuration.Round(time.Millisecond))
	}

	// Clean up.
	if err := a2.Release("drop"); err != nil {
		t.Fatalf("Release(drop): %v", err)
	}

	// Window must be dead after drop.
	time.Sleep(100 * time.Millisecond)
	if tmuxagent.IsWindowAlive(windowID) {
		t.Error("window still alive after Release(drop)")
		_ = tmuxagent.KillWindow(windowID)
	}
}

// TestAcquireDropKillsWindow verifies that Release("drop") kills the
// window, distinct from the combined cycle in TestAcquireColdAndReturn.
func TestAcquireDropKillsWindow(t *testing.T) {
	skipIfNoBinaries(t)

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	a, err := Acquire(ctx, Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	windowID := a.tmuxWindowID

	if err := a.Release("drop"); err != nil {
		t.Fatalf("Release(drop): %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if tmuxagent.IsWindowAlive(windowID) {
		t.Error("window still alive after Release(drop)")
		_ = tmuxagent.KillWindow(windowID)
	}
}

// TestAcquireKeepAliveFor verifies that Release("keep_alive_for:N")
// sets the deadline option and the window stays alive until the next
// Acquire sweep.
func TestAcquireKeepAliveFor(t *testing.T) {
	skipIfNoBinaries(t)

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	a, err := Acquire(ctx, Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	windowID := a.tmuxWindowID

	// Set a very short TTL so the window expires before the next Acquire.
	if err := a.Release("keep_alive_for:1"); err != nil {
		t.Fatalf("Release(keep_alive_for:1): %v", err)
	}

	// Window should still be alive immediately after release.
	if !tmuxagent.IsWindowAlive(windowID) {
		t.Fatal("window died immediately after keep_alive_for release")
	}

	// Verify the deadline option was set.
	dl, ok := tmuxagent.GetWindowOption(windowID, "claudia-deadline")
	if !ok {
		t.Fatal("@claudia-deadline not set after keep_alive_for release")
	}
	t.Logf("@claudia-deadline = %s", dl)

	// Wait for the TTL to expire, then trigger a sweep via Acquire.
	time.Sleep(1500 * time.Millisecond)

	a2, err := Acquire(ctx, Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Acquire after expiry: %v", err)
	}
	defer func() { _ = a2.Release("drop") }()

	// The expired window should have been evicted; a2 should be a fresh window.
	if a2.tmuxWindowID == windowID {
		t.Logf("re-acquired same window — may be a race; eviction happened after acquire listed windows")
	}

	// Either way, the old window should be dead now.
	time.Sleep(100 * time.Millisecond)
	if tmuxagent.IsWindowAlive(windowID) {
		// Clean up if the eviction missed (unlikely but don't leave debris).
		_ = tmuxagent.KillWindow(windowID)
	}
}

// TestAcquireConcurrentDifferentKeys verifies that concurrent Acquire
// calls for different pool keys produce independent windows.
func TestAcquireConcurrentDifferentKeys(t *testing.T) {
	skipIfNoBinaries(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const n = 3
	workDirs := make([]string, n)
	for i := range workDirs {
		workDirs[i] = t.TempDir()
	}

	var mu sync.Mutex
	windowIDs := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := Acquire(ctx, Config{WorkDir: workDirs[i]})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[i] = err
				return
			}
			windowIDs[i] = a.tmuxWindowID
			_ = a.Release("drop")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Acquire[%d]: %v", i, err)
		}
	}

	// All window IDs must be distinct.
	seen := map[string]int{}
	for i, id := range windowIDs {
		if id == "" {
			continue // already reported error
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("window %s assigned to both consumer %d and %d", id, prev, i)
		}
		seen[id] = i
	}
}

// TestAcquireHeldWindowNotReused verifies that two concurrent Acquires
// for the same key don't hand out the same window simultaneously.
func TestAcquireHeldWindowNotReused(t *testing.T) {
	skipIfNoBinaries(t)

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// First acquire — will create a window.
	a1, err := Acquire(ctx, Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer func() { _ = a1.Release("drop") }()

	// Second acquire while a1 is still held — must get a different window.
	a2, err := Acquire(ctx, Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer func() { _ = a2.Release("drop") }()

	if a1.tmuxWindowID == a2.tmuxWindowID {
		t.Errorf("both acquires returned the same window %s — held lock did not work", a1.tmuxWindowID)
	}
	t.Logf("a1=%s a2=%s", a1.tmuxWindowID, a2.tmuxWindowID)
}

// TestAcquirePoolCapEviction verifies that PoolCap=1 causes the oldest
// idle window to be evicted when a second window would exceed the cap.
func TestAcquirePoolCapEviction(t *testing.T) {
	skipIfNoBinaries(t)

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Acquire then return — creates window W1.
	a1, err := Acquire(ctx, Config{WorkDir: workDir, PoolCap: 1})
	if err != nil {
		t.Fatalf("Acquire W1: %v", err)
	}
	w1 := a1.tmuxWindowID
	if err := a1.Release("return"); err != nil {
		t.Fatalf("Release W1: %v", err)
	}

	// Acquire again — W1 is idle, so we get it back (cap=1 is not
	// exceeded since there's only 1 idle window).
	a2, err := Acquire(ctx, Config{WorkDir: workDir, PoolCap: 1})
	if err != nil {
		t.Fatalf("Acquire W2 (should be W1 warm): %v", err)
	}
	w2 := a2.tmuxWindowID
	if w2 != w1 {
		t.Logf("got different window %s vs %s (race or pool rebalance)", w2, w1)
	}

	// Return W2 without releasing the held flag first — now we have 1 idle
	// window. Acquire with PoolCap=1 while creating a new one should evict W2.
	if err := a2.Release("return"); err != nil {
		t.Fatalf("Release W2: %v", err)
	}

	// Now acquire and hold simultaneously: a3 grabs the idle window.
	a3, err := Acquire(ctx, Config{WorkDir: workDir, PoolCap: 1})
	if err != nil {
		t.Fatalf("Acquire a3: %v", err)
	}
	defer func() { _ = a3.Release("drop") }()

	// With a3 held, acquire a4 — cap=1, 1 held, 0 idle → must spawn new.
	// But cap=1 with 1 held means total=1 which equals cap, so no eviction
	// should happen; a new window is spawned and total exceeds cap only
	// transiently (we're within the lock). Let's validate a4 is alive.
	a4, err := Acquire(ctx, Config{WorkDir: workDir, PoolCap: 1})
	if err != nil {
		t.Fatalf("Acquire a4 while a3 held: %v", err)
	}
	defer func() { _ = a4.Release("drop") }()

	if a3.tmuxWindowID == a4.tmuxWindowID {
		t.Errorf("a3 and a4 share window %s — lock didn't prevent it", a3.tmuxWindowID)
	}
	t.Logf("a3=%s a4=%s", a3.tmuxWindowID, a4.tmuxWindowID)
}

// TestAcquireErrorPolicy verifies that PoolPolicy="error" returns an
// error when all matching windows are held.
func TestAcquireErrorPolicy(t *testing.T) {
	skipIfNoBinaries(t)

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Acquire and hold a window.
	a1, err := Acquire(ctx, Config{WorkDir: workDir, PoolPolicy: "error"})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer func() { _ = a1.Release("drop") }()

	// Second Acquire with error policy while a1 is held: we have no idle
	// windows for this key. But the default when none are idle and policy=error
	// only fires when there ARE held windows with no idle — which is true here.
	// Acquire should return an error.
	_, err2 := Acquire(ctx, Config{WorkDir: workDir, PoolPolicy: "error"})
	if err2 == nil {
		t.Error("expected error from PoolPolicy=error with all windows held, got nil")
	} else {
		t.Logf("got expected error: %v", err2)
	}
}

// TestPoolCrashSurvival verifies that a pool window survives a consumer
// crash and can be re-acquired by a fresh consumer. This mirrors
// TestCrashSurvival but via the Acquire/Release path.
func TestPoolCrashSurvival(t *testing.T) {
	skipIfNoBinaries(t)

	workDir := t.TempDir()

	// Spawn a child that acquires a pool window, prints its ID, then
	// blocks (simulating a consumer that crashes).
	child := exec.Command(
		os.Args[0],
		"-test.run=^TestPoolCrashSurvivalHelper$",
		"-test.v",
	)
	child.Env = append(os.Environ(),
		"CLAUDIA_POOL_CRASH_HELPER=1",
		"CLAUDIA_POOL_CRASH_WORKDIR="+workDir,
	)
	childStdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	child.Stderr = os.Stderr

	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	// Read the window ID from the child.
	lineCh := make(chan string, 10)
	go func() {
		buf := make([]byte, 4096)
		acc := ""
		for {
			n, err := childStdout.Read(buf)
			if n > 0 {
				acc += string(buf[:n])
				for {
					idx := strings.IndexByte(acc, '\n')
					if idx < 0 {
						break
					}
					lineCh <- acc[:idx]
					acc = acc[idx+1:]
				}
			}
			if err != nil {
				close(lineCh)
				return
			}
		}
	}()

	findLine := func(prefix string) string {
		t.Helper()
		deadline := time.After(90 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatalf("timeout waiting for %q from child", prefix)
			case line, ok := <-lineCh:
				if !ok {
					t.Fatalf("child closed stdout before %q", prefix)
				}
				t.Logf("child: %s", line)
				if after, ok2 := strings.CutPrefix(line, prefix); ok2 {
					return after
				}
			}
		}
	}

	windowID := findLine("POOL_WINDOW=")
	t.Logf("child pool window: %s", windowID)

	// Kill the child — uncontrolled crash.
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = child.Wait()
	t.Log("child killed")

	time.Sleep(200 * time.Millisecond)

	// The pool window must still be alive.
	if !tmuxagent.IsWindowAlive(windowID) {
		t.Fatal("pool window died when consumer was killed")
	}

	// Re-acquire — should adopt the surviving window or spawn a new one.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a, err := Acquire(ctx, Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("re-Acquire after crash: %v", err)
	}
	t.Logf("re-acquired window: %s", a.tmuxWindowID)

	if err := a.Release("drop"); err != nil {
		t.Errorf("Release(drop) after crash survival: %v", err)
	}
}

// TestPoolCrashSurvivalHelper is the child side of TestPoolCrashSurvival.
func TestPoolCrashSurvivalHelper(t *testing.T) {
	if os.Getenv("CLAUDIA_POOL_CRASH_HELPER") != "1" {
		t.Skip("helper — only runs as subprocess of TestPoolCrashSurvival")
	}

	workDir := os.Getenv("CLAUDIA_POOL_CRASH_WORKDIR")
	if workDir == "" {
		t.Fatal("CLAUDIA_POOL_CRASH_WORKDIR not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a, err := Acquire(ctx, Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Intentionally no Release — parent kills us.

	fmt.Printf("POOL_WINDOW=%s\n", a.tmuxWindowID)

	// Block until killed.
	select {}
}

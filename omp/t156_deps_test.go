// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestDependencyLockHonorsCancelledCaller(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	modules := filepath.Join(dir, "node_modules")
	if err := os.Mkdir(modules, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(modules, depsLock), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// The old blocking flock never returns until the competing install exits,
	// even though this request is already cancelled. No fixture timer decides.
	if err := EnsureDeps(ctx, filepath.Join(dir, "auth.ts")); !errors.Is(err, context.Canceled) {
		t.Fatalf("dependency lock = %v", err)
	}
}

// 🎯T156: a checkout with no sidecar install gets one before bun starts the
// script, once per lockfile; a failed install is an error at once that
// names it, not a sidecar that never becomes ready.
func TestT156SidecarInstallsItsOwnDependencies(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "server.ts")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("server.ts", "")
	write("package.json", `{"dependencies":{"x":"1"}}`)
	write("bun.lockb", "lock-1")

	var installs atomic.Int32
	var fail atomic.Bool
	prev := depsInstall
	t.Cleanup(func() { depsInstall = prev })
	depsInstall = func(_ context.Context, got string) ([]byte, error) {
		if got != dir {
			t.Errorf("install ran in %s, want %s", got, dir)
		}
		if fail.Load() {
			return []byte("error: lockfile had changes, but lockfile is frozen"), errors.New("exit status 1")
		}
		installs.Add(1)
		return nil, nil
	}
	ctx := context.Background()

	// Concurrent starts in a fresh checkout install once.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := EnsureDeps(ctx, script); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := installs.Load(); n != 1 {
		t.Fatalf("installs in a fresh checkout = %d, want 1", n)
	}
	if err := EnsureDeps(ctx, script); err != nil || installs.Load() != 1 {
		t.Fatalf("a current install ran again: err=%v installs=%d", err, installs.Load())
	}

	// A pull that changes the lockfile reinstalls.
	write("bun.lockb", "lock-2")
	if err := EnsureDeps(ctx, script); err != nil || installs.Load() != 2 {
		t.Fatalf("a changed lockfile: err=%v installs=%d, want 2", err, installs.Load())
	}

	// A failed install says what failed, and is retried next time.
	write("bun.lockb", "lock-3")
	fail.Store(true)
	err := EnsureDeps(ctx, script)
	if err == nil || !strings.Contains(err.Error(), "bun install --frozen-lockfile") ||
		!strings.Contains(err.Error(), "lockfile is frozen") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("failed install error = %v", err)
	}
	fail.Store(false)
	if err := EnsureDeps(ctx, script); err != nil || installs.Load() != 3 {
		t.Fatalf("retry after a failed install: err=%v installs=%d, want 3", err, installs.Load())
	}

	// A script with no package.json beside it needs nothing.
	bundle := filepath.Join(t.TempDir(), "server.js")
	if err := EnsureDeps(ctx, bundle); err != nil || installs.Load() != 3 {
		t.Fatalf("a bundled script: err=%v installs=%d", err, installs.Load())
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSeparateProcessesCannotOverlapLiveGate(t *testing.T) {
	// No provider flags or real live suite: only subprocesses running shell
	// sentinels. Both wrappers share a state home but not a working directory.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "livelock")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	state := filepath.Join(t.TempDir(), "state")
	firstDir, secondDir := t.TempDir(), t.TempDir()
	entered, release := filepath.Join(firstDir, "entered"), filepath.Join(firstDir, "release")
	secondEntered := filepath.Join(secondDir, "entered")
	env := append(os.Environ(), "XDG_STATE_HOME="+state)
	first := exec.CommandContext(ctx, bin, "--", "sh", "-c", `echo yes > "$1"; while [ ! -f "$2" ]; do sleep .05; done`, "sh", entered, release)
	first.Dir, first.Env = firstDir, env
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		if !waited {
			_ = first.Wait()
		}
	}()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("first child did not enter")
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := exec.CommandContext(ctx, bin, "--", "sh", "-c", `echo yes > "$1"`, "sh", secondEntered)
	second.Dir, second.Env = secondDir, env
	out, err := second.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "live gate busy: another make live holds") || !strings.Contains(string(out), "no live tests started") {
		t.Fatalf("second should refuse before child: err=%v output=%s", err, out)
	}
	if _, err := os.Stat(secondEntered); !os.IsNotExist(err) {
		t.Fatalf("second child ran despite contention: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	// Defer must not call Wait twice.
	waited = true
	third := exec.CommandContext(ctx, bin, "--", "sh", "-c", `echo yes > "$1"`, "sh", secondEntered)
	third.Dir, third.Env = secondDir, env
	out, err = third.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "wait 0s") {
		t.Fatalf("third should enter: err=%v output=%s", err, out)
	}
	if _, err := os.Stat(secondEntered); err != nil {
		t.Fatalf("third child never entered: %v", err)
	}
	// The lockfile remains on disk, but it is not a stale occupancy signal.
	if _, err := os.Stat(filepath.Join(state, "claudia", lockName)); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedHolderReleasesOnlyAfterChildEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "livelock")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	state := filepath.Join(t.TempDir(), "state")
	entered := filepath.Join(t.TempDir(), "entered")
	env := append(os.Environ(), "XDG_STATE_HOME="+state)
	first := exec.CommandContext(ctx, bin, "--", "sh", "-c", `echo yes > "$1"; sleep 30`, "sh", entered)
	first.Env = env
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = first.Process.Kill()
			_ = first.Wait()
		}
	}()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("first child did not enter")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err == nil {
		t.Fatal("interrupted gate reported success")
	}
	waited = true
	third := exec.CommandContext(ctx, bin, "--", "true")
	third.Env = env
	if out, err := third.CombinedOutput(); err != nil {
		t.Fatalf("lock not released after signal cleanup: %v %s", err, out)
	}
}

func TestKilledSupervisorDoesNotFreeRunningChildLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "livelock")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	state := filepath.Join(t.TempDir(), "state")
	entered, release := filepath.Join(t.TempDir(), "entered"), filepath.Join(t.TempDir(), "release")
	env := append(os.Environ(), "XDG_STATE_HOME="+state)
	first := exec.CommandContext(ctx, bin, "--", "sh", "-c", `echo yes > "$1"; while [ ! -f "$2" ]; do sleep .05; done`, "sh", entered, release)
	first.Env = env
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(release, nil, 0o600)
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(until) {
			_ = first.Process.Kill()
			_ = first.Wait()
			t.Fatal("first child did not enter")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()
	second := exec.CommandContext(ctx, bin, "--", "true")
	second.Env = env
	if out, err := second.CombinedOutput(); err == nil || !strings.Contains(string(out), "live gate busy") {
		t.Fatalf("child lost lock when supervisor was killed: %v %s", err, out)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(5 * time.Second)
	for {
		third := exec.CommandContext(ctx, bin, "--", "true")
		third.Env = env
		if _, err := third.CombinedOutput(); err == nil {
			return
		}
		if time.Now().After(until) {
			t.Fatal("child did not release lock after exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStateDirectoryFailureNeverRunsChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "livelock")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entered := filepath.Join(t.TempDir(), "entered")
	child := exec.CommandContext(ctx, bin, "--", "sh", "-c", `echo yes > "$1"`, "sh", entered)
	child.Env = append(os.Environ(), "XDG_STATE_HOME="+file)
	if out, err := child.CombinedOutput(); err == nil || !strings.Contains(string(out), "live gate lock:") {
		t.Fatalf("fail closed: %v %s", err, out)
	}
	if _, err := os.Stat(entered); !os.IsNotExist(err) {
		t.Fatalf("child ran: %v", err)
	}
}

func TestMakeLiveUsesHostLock(t *testing.T) {
	out, err := exec.Command("make", "-C", "../..", "-n", "live").CombinedOutput()
	if err != nil {
		t.Fatalf("make -n live: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "go run ./cmd/livelock -- go test -count=1") {
		t.Fatalf("make live bypasses the host lock:\n%s", out)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// t166HelperEnv makes the test binary a stand-in broker: it starts a
// sidecar with Ensure and then waits to be killed.
const t166HelperEnv = "CLAUDIA_T166_HELPER"

func TestT166Helper(t *testing.T) {
	if os.Getenv(t166HelperEnv) == "" {
		t.Skip("helper process only")
	}
	if _, err := Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("ready\n")
	select {}
}

// t166Gone waits until pid has exited, for at most limit.
func t166Gone(pid int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func t166Parent(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps %d: %v", pid, err)
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("ps %d: %q", pid, out)
	}
	return ppid
}

// 🎯T166: the sidecar is the child of the process that started it and dies
// with it, however that process dies. A broker killed outright leaves no
// orphan for the next broker to adopt.
func TestT166SidecarDiesWithTheProcessThatStartedIt(t *testing.T) {
	socket := t145Env(t)
	ctx := wallclockguard.UntilTestTimeout(t)
	helper := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestT166Helper$", "-test.v")
	helper.Env = append(os.Environ(), t166HelperEnv+"=1")
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = helper.Process.Kill(); _ = helper.Wait() })
	buf := make([]byte, 4096)
	var seen string
	for !strings.Contains(seen, "ready") {
		n, err := out.Read(buf)
		if err != nil {
			t.Fatalf("helper ended before its sidecar was ready: %v\n%s", err, seen)
		}
		seen += string(buf[:n])
	}
	sidecar := t145Pid(t, socket)
	if got := t166Parent(t, sidecar); got != helper.Process.Pid {
		t.Fatalf("sidecar %d has parent %d, want the process that started it (%d)", sidecar, got, helper.Process.Pid)
	}
	t145Serves(ctx, t, socket)

	// SIGKILL: no handler runs, so only the kernel closing the lifeline
	// can tell the sidecar.
	if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	if !t166Gone(sidecar, 10*time.Second) {
		t.Fatalf("sidecar %d outlived the process that started it", sidecar)
	}
	if Listening(ctx, socket) {
		t.Fatal("something still serves the socket")
	}
}

// 🎯T166: a broker stops a sidecar it did not start, rather than adopting
// it, and leaves its own alone.
func TestT166StopUnownedReplacesALeftoverSidecar(t *testing.T) {
	socket := t145Env(t)
	ctx := wallclockguard.UntilTestTimeout(t)

	if pid, err := StopUnowned(ctx); err != nil || pid != 0 {
		t.Fatalf("StopUnowned with no sidecar = %d, %v; want 0, nil", pid, err)
	}

	// A leftover: started outside this process, with no lifeline, as an
	// earlier broker's sidecar is.
	left := exec.Command("bun", ServerScript(), socket)
	left.Dir = filepath.Dir(ServerScript())
	if err := left.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = left.Process.Kill(); _ = left.Wait() })
	for !Listening(ctx, socket) {
		time.Sleep(20 * time.Millisecond)
	}
	waited := make(chan struct{})
	go func() { _ = left.Wait(); close(waited) }()

	pid, err := StopUnowned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pid != left.Process.Pid {
		t.Fatalf("StopUnowned stopped %d, want the leftover %d", pid, left.Process.Pid)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the leftover sidecar is still running")
	}

	if _, err := Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = StopSidecar(socket) })
	own := t145Pid(t, socket)
	if got := t166Parent(t, own); got != os.Getpid() {
		t.Fatalf("sidecar %d has parent %d, want this process %d", own, got, os.Getpid())
	}
	if pid, err := StopUnowned(ctx); err != nil || pid != 0 {
		t.Fatalf("StopUnowned on this process's own sidecar = %d, %v; want 0, nil", pid, err)
	}
	t145Serves(ctx, t, socket)
}

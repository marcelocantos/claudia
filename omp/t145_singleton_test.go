// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func t145Env(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(ServerScript()); err != nil {
		t.Skip("sidecar/server.ts missing")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not installed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ServerScript()), "node_modules")); err != nil {
		t.Skip("sidecar dependencies not installed (bun install in sidecar/)")
	}
	dir, err := os.MkdirTemp("/tmp", "omp-t145-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "omp.sock")
	t.Setenv(SocketEnv, socket)
	t.Setenv("JEVONS_SPOOL_DIR", filepath.Join(dir, "spool"))
	return socket
}

func t145Pid(t *testing.T, socket string) int {
	t.Helper()
	raw, err := os.ReadFile(pidPath(socket))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// t145Serves proves a live sidecar answers on socket.
func t145Serves(ctx context.Context, t *testing.T, socket string) {
	t.Helper()
	conn, err := Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Send(Message{Op: OpLoad, Seat: "t145", Provider: XAIOAuth, Model: "grok-4.6", Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	ev, err := conn.Recv()
	if err != nil || ev.Type != "ready" {
		t.Fatalf("load = %+v, %v; want ready", ev, err)
	}
}

// 🎯T145: only one sidecar ever serves a socket. A second start on a live
// socket exits without touching it; the first keeps serving and keeps the
// pid file. After the owner is killed outright its stale socket file is
// taken over by the next start.
func TestT145OnlyOneSidecarServesASocket(t *testing.T) {
	socket := t145Env(t)
	ctx := wallclockguard.UntilTestTimeout(t)
	if _, err := Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = StopSidecar(socket) })
	first := t145Pid(t, socket)

	// A redundant start: the case that orphaned the running sidecar on
	// 2026-09-29, when every start unlinked the socket before binding.
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "bun", ServerScript(), socket)
	cmd.Dir = filepath.Dir(ServerScript())
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil { // it must exit, and cleanly
		t.Fatalf("second sidecar: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "served by another sidecar") {
		t.Fatalf("second sidecar did not say why it left: %q", stderr.String())
	}
	t145Serves(ctx, t, socket)
	if got := t145Pid(t, socket); got != first {
		t.Fatalf("pid file = %d, want the serving sidecar %d", got, first)
	}

	// Killed outright, the owner leaves its socket file behind; the kernel
	// released its lock, so the next start takes the socket over.
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for syscall.Kill(first, 0) == nil {
		// Waits for the pid to be gone; `go test -timeout` is the clock.
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("expected a stale socket file after kill -9: %v", err)
	}
	if _, err := Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if second := t145Pid(t, socket); second == first {
		t.Fatal("the pid file still names the killed sidecar")
	}
	t145Serves(ctx, t, socket)
}

// Concurrent starts on an empty socket end with exactly one sidecar.
func TestT145ConcurrentStartsLeaveOneSidecar(t *testing.T) {
	socket := t145Env(t)
	ctx := wallclockguard.UntilTestTimeout(t)
	const n = 4
	cmds := make([]*exec.Cmd, n)
	for i := range cmds {
		cmds[i] = exec.Command("bun", ServerScript(), socket)
		cmds[i].Dir = filepath.Dir(ServerScript())
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	exited := make(chan int, n)
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Wait(); exited <- i }()
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
		}
		wg.Wait()
	})
	// n-1 of them must leave; `go test -timeout` is the clock.
	left := map[int]bool{}
	for range n - 1 {
		left[<-exited] = true
	}
	for !Listening(ctx, socket) {
		select {
		case i := <-exited:
			t.Fatalf("every sidecar exited; the last was %d", cmds[i].Process.Pid)
		default:
		}
	}
	t145Serves(ctx, t, socket)
	owner := t145Pid(t, socket)
	for i, c := range cmds {
		if left[i] {
			continue
		}
		if c.Process.Pid != owner {
			t.Fatalf("the sidecar still running (%d) is not the socket's owner %d", c.Process.Pid, owner)
		}
		if syscall.Kill(owner, 0) != nil {
			t.Fatal("the owner is not alive")
		}
	}
	select {
	case i := <-exited:
		t.Fatalf("the owning sidecar %d exited too", cmds[i].Process.Pid)
	default:
	}
}

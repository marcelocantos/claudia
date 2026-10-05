// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// livelock serializes make live across checkouts on the same host. It deliberately
// does not lock the (per-worktree) git directory or the (per-test) plan cache.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"golang.org/x/sys/unix"
)

const lockName = "live.lock"

func run(args []string) error {
	if len(args) < 2 || args[0] != "--" {
		return fmt.Errorf("usage: livelock -- command [args...]")
	}
	dir, err := broker.StateDir()
	if err != nil {
		return fmt.Errorf("live gate lock: state directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("live gate lock: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, lockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("live gate lock: open %s: %w", path, err)
	}
	defer f.Close()
	// Do not unlink or replace this file: an old holder and a new caller could
	// otherwise lock different inodes and both enter the live suite.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("live gate busy: another make live holds %s; no live tests started", path)
		}
		return fmt.Errorf("live gate lock: acquire %s: %w", path, err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	fmt.Fprintf(os.Stderr, "live gate acquired %s (wait 0s)\n", path)

	cmd := exec.Command(args[1], args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// If the supervisor itself is killed, go test retains the lock until it
	// exits. On ordinary signals we stop the entire child process group and
	// wait for it before releasing the lock.
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("live gate: start %s: %w", args[1], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case sig := <-sigs:
		// A second signal does not release the lock early. A child ignoring
		// TERM is escalated after a bounded grace, then reaped.
		_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		return fmt.Errorf("live gate interrupted: %s", sig)
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}

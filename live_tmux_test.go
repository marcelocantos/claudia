// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tmuxSocketEnv is tmuxagent's socket override, named here because the
// suite has to set it before any agent resolves a socket path.
const tmuxSocketEnv = "CLAUDIA_TMUX_SOCKET"

// planTestTmuxSocket decides whether this test process runs its tmux
// seats on a server of its own, and where that server's socket goes.
//
// A seat started on the shared claudia socket
// ($XDG_STATE_HOME/claudia/tmux.sock) is a window with no fleet registry
// entry, and jevonsd's pane census reaps exactly that shape: "no registry
// entry and no in-flight turn". On 2026-09-17 it killed in-process test
// seats at 19:07:12, 19:07:42, 19:10:12, 19:13:12 and 19:13:42
// (~/.local/var/log/jevonsd.log), which reached the suite as
// "WaitReady: claude not ready (no_composer): no idle input box after 30s"
// and, when the kill landed mid-turn, "WaitForResponse: context deadline
// exceeded" 180 seconds in. Neither failure names tmux, so the suite reads
// as a broken provider rather than a reaped window.
//
// The daemon package bought its immunity per-test in 10e7e44. The root
// package buys it once, here, so a live test added later is immune without
// having to know any of this. A caller-set CLAUDIA_TMUX_SOCKET is the
// caller's and is left alone.
func planTestTmuxSocket(lookup func(string) (string, bool), dir string) (socket string, private bool) {
	if s, ok := lookup(tmuxSocketEnv); ok && strings.TrimSpace(s) != "" {
		return "", false
	}
	return filepath.Join(dir, "tmux.sock"), true
}

// usePrivateTmuxServer points the suite at its own tmux server and returns
// the function that kills it. The directory is short and under /tmp
// because a unix socket path is capped near 104 bytes and the system temp
// dir on macOS is long enough to blow it.
func usePrivateTmuxServer() func() {
	dir, err := os.MkdirTemp("/tmp", "clt")
	if err != nil {
		panic(err)
	}
	socket, private := planTestTmuxSocket(os.LookupEnv, dir)
	if !private {
		_ = os.RemoveAll(dir)
		return func() {}
	}
	_ = os.Setenv(tmuxSocketEnv, socket)
	return func() { stopPrivateTmuxServer(socket, dir) }
}

// stopPrivateTmuxServer removes the private socket directory. It does not
// run tmux, including when the socket file is already there.
func stopPrivateTmuxServer(socket, dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// sharedClaudiaTmuxSocket is the socket an agent resolves with no
// override — the one the fleet's census watches.
func sharedClaudiaTmuxSocket() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(stateHome, "claudia", "tmux.sock")
}

// TestSeatsRunOnAPrivateTmuxServer is 🎯T77's standing enforcement. It runs
// in the hermetic gate as well as under a live gate, so a change that drops
// the TestMain wiring is caught by `make gate` and not only by a live run
// that happens to overlap a census tick.
func TestPrivateTmuxCleanupLeavesTmuxUnstarted(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "clt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	marker := filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "tmux.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stopPrivateTmuxServer(sock, "")
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("cleanup executed tmux: %v", statErr)
	}
}

func TestSeatsRunOnAPrivateTmuxServer(t *testing.T) {
	got := os.Getenv(tmuxSocketEnv)
	if strings.TrimSpace(got) == "" {
		t.Fatal("suite did not set a private tmux socket")
	}
	if shared := sharedClaudiaTmuxSocket(); got == shared {
		t.Fatalf("suite is on the shared claudia tmux socket %s; jevonsd's pane census reaps unregistered windows there", shared)
	}
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("private tmux socket directory is not there: %v", err)
	}
}

func TestPlanTestTmuxSocket(t *testing.T) {
	const dir = "/tmp/clt0"
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}

	t.Run("unset gets a private socket", func(t *testing.T) {
		socket, private := planTestTmuxSocket(env(nil), dir)
		if !private {
			t.Fatal("an unset override must yield a private server")
		}
		if want := filepath.Join(dir, "tmux.sock"); socket != want {
			t.Fatalf("socket=%q want %q", socket, want)
		}
	})

	t.Run("blank gets a private socket", func(t *testing.T) {
		if _, private := planTestTmuxSocket(env(map[string]string{tmuxSocketEnv: "  "}), dir); !private {
			t.Fatal("a blank override is not a caller's choice of socket")
		}
	})

	// The caller's socket is honoured: the daemon suite sets this per-test
	// and must keep the server it made, not one TestMain made.
	t.Run("caller socket is left alone", func(t *testing.T) {
		socket, private := planTestTmuxSocket(env(map[string]string{tmuxSocketEnv: "/tmp/mine/tmux.sock"}), dir)
		if private || socket != "" {
			t.Fatalf("socket=%q private=%v; an explicit override must be left alone", socket, private)
		}
	})
}
